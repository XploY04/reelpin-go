package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/XploY04/reelpin-go/internal/ai"
	"github.com/XploY04/reelpin-go/internal/geo"
	"github.com/XploY04/reelpin-go/internal/platform"
	"github.com/XploY04/reelpin-go/internal/queue"
	"github.com/XploY04/reelpin-go/internal/sourceidentity"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Deps are the seams. Everything that costs money, touches a provider or writes
// a file is behind one, so the whole pipeline runs in tests with fakes.
type Deps struct {
	Pool        *pgxpool.Pool
	Handlers    *platform.Registry
	Transcriber ai.Transcriber
	ImageReader ai.ImageReader
	Extractor   ai.Extractor
	Categorizer ai.Categorizer
	Geocoder    geo.Geocoder
	Logger      *slog.Logger
	// TempRoot is where each run gets its own directory. The pipeline deletes
	// it on every exit, and sweeps leftovers on startup.
	TempRoot string
	Now      func() time.Time
}

type Pipeline struct {
	deps        Deps
	checkpoints *Checkpoints
}

func New(deps Deps) *Pipeline {
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if deps.TempRoot == "" {
		deps.TempRoot = filepath.Join(os.TempDir(), "reelpin-runs")
	}
	return &Pipeline{deps: deps, checkpoints: NewCheckpoints(deps.Pool)}
}

// run is everything the stages share for one message.
type run struct {
	ID        string
	ContentID string
	Identity  sourceidentity.SourceIdentity
	WorkDir   string

	Prepared   platform.Prepared
	Transcript string
	Caption    string
	Extraction ai.Extraction
	VersionID  string
}

// Process runs one message to completion. It is safe to call twice with the
// same message: every stage is checkpointed, and the final writes are keyed.
func (p *Pipeline) Process(ctx context.Context, message queue.Message) error {
	state, err := p.load(ctx, message.RunID)
	if err != nil {
		return err
	}

	// One directory per run, removed however this function exits: success,
	// failure, or cancellation.
	workDir, err := os.MkdirTemp(p.deps.TempRoot, "run-"+shortID(state.ID)+"-")
	if err != nil {
		return fmt.Errorf("creating the run directory: %w", err)
	}
	state.WorkDir = workDir
	defer os.RemoveAll(workDir)

	for _, stage := range Stages {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := p.checkpoints.Progress(ctx, state.ID, stage); err != nil {
			return err
		}
		if err := p.runStage(ctx, stage, state); err != nil {
			return err
		}
	}
	return nil
}

func (p *Pipeline) runStage(ctx context.Context, stage string, state *run) error {
	switch stage {
	case StagePrepare:
		return p.prepare(ctx, state)
	case StageTranscribeOrOCR:
		return p.transcribeOrRead(ctx, state)
	case StageExtract:
		return p.extract(ctx, state)
	case StageGeocode:
		return p.geocode(ctx, state)
	case StagePersistContent:
		return p.persistContent(ctx, state)
	case StagePersonalize:
		return p.personalize(ctx, state)
	case StageSave:
		return p.save(ctx, state)
	case StageEmitIndex:
		return p.emit(ctx, state, "content.index", queue.QueuePersonalize)
	case StageEmitNotifications:
		return p.emit(ctx, state, "content.notify", queue.QueueNotify)
	case StageComplete:
		return p.complete(ctx, state)
	default:
		return fmt.Errorf("unknown stage %q", stage)
	}
}

func (p *Pipeline) load(ctx context.Context, runID string) (*run, error) {
	var (
		state       run
		platformID  string
		contentType string
		contentID   *string
		normalized  string
	)
	err := p.deps.Pool.QueryRow(ctx, `
		SELECT r.id::text, c.id::text, c.source_platform, c.source_content_type,
		       c.source_content_id, c.normalized_url
		FROM reelpin.processing_runs r
		JOIN reelpin.contents c ON c.id = r.content_id
		WHERE r.id = $1`, runID,
	).Scan(&state.ID, &state.ContentID, &platformID, &contentType, &contentID, &normalized)
	if errors.Is(err, pgx.ErrNoRows) {
		// The run is gone: the message describes work that no longer exists.
		return nil, EmptyPostContent(fmt.Errorf("run %s not found", runID))
	}
	if err != nil {
		return nil, fmt.Errorf("loading the run: %w", err)
	}

	state.Identity = sourceidentity.SourceIdentity{
		NormalizedURL: normalized,
		OriginalURL:   normalized,
		Platform:      platformID,
		ContentType:   contentType,
	}
	if contentID != nil {
		state.Identity.ContentID = *contentID
	}
	return &state, nil
}

func (p *Pipeline) prepare(ctx context.Context, state *run) error {
	hash := InputHash(state.Identity.NormalizedURL, state.Identity.Platform)

	var cached platform.Prepared
	if found, err := p.checkpoints.Load(ctx, state.ID, StagePrepare, hash, &cached); err != nil {
		return err
	} else if found {
		// The media itself is gone with the old temp directory, but the text
		// and the URLs it produced are what the later stages need.
		state.Prepared = cached
		state.Caption = cached.Caption
		state.Transcript = cached.Transcript
		return nil
	}

	handler, err := p.deps.Handlers.For(state.Identity)
	if err != nil {
		return UnsupportedPostType(err)
	}

	identity, err := handler.Normalize(ctx, state.Identity)
	if err != nil {
		return Classify(err)
	}
	state.Identity = identity

	prepared, err := handler.Prepare(ctx, identity, state.WorkDir)
	if err != nil {
		return Classify(err)
	}
	state.Prepared = prepared
	state.Caption = prepared.Caption
	state.Transcript = prepared.Transcript

	return p.checkpoints.Save(ctx, state.ID, StagePrepare, hash, prepared)
}

// transcribeOrRead fills in the text the platform did not supply: speech from
// audio, or the words in a post's images.
func (p *Pipeline) transcribeOrRead(ctx context.Context, state *run) error {
	if strings.TrimSpace(state.Transcript) != "" {
		return nil
	}

	hash := InputHash(state.Identity.NormalizedURL, state.Prepared.AudioPath,
		strings.Join(state.Prepared.ImagePaths, "|"))

	var cached string
	if found, err := p.checkpoints.Load(ctx, state.ID, StageTranscribeOrOCR, hash, &cached); err != nil {
		return err
	} else if found {
		state.Transcript = cached
		return nil
	}

	switch {
	case state.Prepared.AudioPath != "":
		transcript, err := p.deps.Transcriber.Transcribe(ctx, ai.Media{
			Path: state.Prepared.AudioPath, MIMEType: "audio/mpeg",
		})
		if err != nil {
			return Classify(err)
		}
		state.Transcript = transcript

	case len(state.Prepared.ImagePaths) > 0:
		images := make([]ai.Media, 0, len(state.Prepared.ImagePaths))
		for _, path := range state.Prepared.ImagePaths {
			images = append(images, ai.Media{Path: path, MIMEType: "image/jpeg"})
		}
		text, err := p.deps.ImageReader.ReadText(ctx, images)
		if err != nil {
			return Classify(err)
		}
		state.Transcript = text
	}

	// A post with no speech, no image text and no caption has nothing to save.
	if strings.TrimSpace(state.Transcript) == "" && strings.TrimSpace(state.Caption) == "" {
		return EmptyPostContent(errors.New("no transcript, image text or caption"))
	}

	return p.checkpoints.Save(ctx, state.ID, StageTranscribeOrOCR, hash, state.Transcript)
}

func (p *Pipeline) extract(ctx context.Context, state *run) error {
	hash := InputHash(ai.SchemaVersion, state.Transcript, state.Caption)

	var cached ai.Extraction
	if found, err := p.checkpoints.Load(ctx, state.ID, StageExtract, hash, &cached); err != nil {
		return err
	} else if found {
		state.Extraction = cached
		return nil
	}

	extraction, err := p.deps.Extractor.Extract(ctx, state.Transcript, state.Caption)
	if err != nil {
		return Classify(err)
	}
	state.Extraction = extraction.Normalize()

	return p.checkpoints.Save(ctx, state.ID, StageExtract, hash, state.Extraction)
}

// geocode resolves the places the extraction found. A place that cannot be
// resolved is kept without coordinates: the fact is still worth saving, it just
// does not go on the map.
func (p *Pipeline) geocode(ctx context.Context, state *run) error {
	if len(state.Extraction.Locations) == 0 {
		return nil
	}

	encoded, err := json.Marshal(state.Extraction.Locations)
	if err != nil {
		return fmt.Errorf("encoding locations: %w", err)
	}
	hash := InputHash(string(encoded))

	var cached []ai.Location
	if found, err := p.checkpoints.Load(ctx, state.ID, StageGeocode, hash, &cached); err != nil {
		return err
	} else if found {
		state.Extraction.Locations = cached
		return nil
	}

	for index, location := range state.Extraction.Locations {
		if location.Latitude != nil && location.Longitude != nil {
			continue
		}
		for _, query := range geo.Queries(location.Name, location.Neighborhood,
			location.City, location.State, location.Country) {
			point, err := p.deps.Geocoder.Geocode(ctx, query)
			if errors.Is(err, geo.ErrNotFound) {
				continue
			}
			if err != nil {
				// A geocoder outage must not cost the save.
				p.deps.Logger.Warn("geocoding failed", "run_id", state.ID, "error", err)
				break
			}
			latitude, longitude := point.Latitude, point.Longitude
			state.Extraction.Locations[index].Latitude = &latitude
			state.Extraction.Locations[index].Longitude = &longitude
			break
		}
	}

	return p.checkpoints.Save(ctx, state.ID, StageGeocode, hash, state.Extraction.Locations)
}

func shortID(value string) string {
	if len(value) > 8 {
		return value[:8]
	}
	return value
}
