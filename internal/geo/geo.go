// Package geo owns coordinates and the rules a point must satisfy before it
// reaches the database. Validation lives here rather than in SQL so a bad
// coordinate is a typed error a handler can explain, not a constraint
// violation a user sees as an internal failure.
package geo

import (
	"errors"
	"fmt"
	"math"
)

var (
	// ErrOutOfRange is a coordinate outside the world.
	ErrOutOfRange = errors.New("coordinate is out of range")
	// ErrNotFinite is NaN or an infinity, which JSON can carry and SQL cannot.
	ErrNotFinite = errors.New("coordinate is not a finite number")
	// ErrEmptyBounds is a bounding box with no area.
	ErrEmptyBounds = errors.New("bounds have no area")
)

// Point is one place on Earth.
type Point struct {
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
}

// NewPoint validates before constructing, so an invalid Point cannot exist.
func NewPoint(latitude, longitude float64) (Point, error) {
	if math.IsNaN(latitude) || math.IsNaN(longitude) ||
		math.IsInf(latitude, 0) || math.IsInf(longitude, 0) {
		return Point{}, ErrNotFinite
	}
	if latitude < -90 || latitude > 90 {
		return Point{}, fmt.Errorf("%w: latitude %g is outside -90..90", ErrOutOfRange, latitude)
	}
	if longitude < -180 || longitude > 180 {
		return Point{}, fmt.Errorf("%w: longitude %g is outside -180..180", ErrOutOfRange, longitude)
	}
	return Point{Latitude: latitude, Longitude: longitude}, nil
}

// Bounds is a viewport. West may be greater than East: that is a box crossing
// the antimeridian, which is a real thing a user can pan to and not an error.
type Bounds struct {
	South float64 `json:"south"`
	West  float64 `json:"west"`
	North float64 `json:"north"`
	East  float64 `json:"east"`
}

func NewBounds(south, west, north, east float64) (Bounds, error) {
	southWest, err := NewPoint(south, west)
	if err != nil {
		return Bounds{}, err
	}
	northEast, err := NewPoint(north, east)
	if err != nil {
		return Bounds{}, err
	}
	if southWest.Latitude >= northEast.Latitude {
		return Bounds{}, fmt.Errorf("%w: south %g is not below north %g",
			ErrEmptyBounds, south, north)
	}
	return Bounds{South: south, West: west, North: north, East: east}, nil
}

// CrossesAntimeridian is true for a box spanning the 180th meridian, where west
// is numerically greater than east. Such a box is two boxes in SQL, and a query
// that forgets this silently returns the whole world instead of the Pacific.
func (b Bounds) CrossesAntimeridian() bool {
	return b.West > b.East
}
