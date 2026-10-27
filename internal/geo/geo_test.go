package geo

import (
	"errors"
	"math"
	"testing"
)

func TestAPointMustBeOnEarth(t *testing.T) {
	if _, err := NewPoint(15.58, 73.74); err != nil {
		t.Fatalf("a real place was rejected: %v", err)
	}
	for _, tt := range []struct {
		name                string
		latitude, longitude float64
		want                error
	}{
		{"latitude too high", 91, 0, ErrOutOfRange},
		{"latitude too low", -91, 0, ErrOutOfRange},
		{"longitude too high", 0, 181, ErrOutOfRange},
		{"longitude too low", 0, -181, ErrOutOfRange},
		{"not a number", math.NaN(), 0, ErrNotFinite},
		{"infinite", 0, math.Inf(1), ErrNotFinite},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NewPoint(tt.latitude, tt.longitude); !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestTheCornersOfTheWorldAreValid(t *testing.T) {
	for _, tt := range []struct{ latitude, longitude float64 }{
		{90, 180}, {-90, -180}, {0, 0},
	} {
		if _, err := NewPoint(tt.latitude, tt.longitude); err != nil {
			t.Errorf("(%g, %g) was rejected: %v", tt.latitude, tt.longitude, err)
		}
	}
}

func TestBoundsNeedArea(t *testing.T) {
	if _, err := NewBounds(15, 73, 16, 74); err != nil {
		t.Fatalf("a normal box was rejected: %v", err)
	}
	// South at or above north is not a viewport, it is a mistake.
	for _, tt := range []struct{ south, north float64 }{{16, 15}, {15, 15}} {
		if _, err := NewBounds(tt.south, 73, tt.north, 74); !errors.Is(err, ErrEmptyBounds) {
			t.Errorf("south %g north %g was accepted", tt.south, tt.north)
		}
	}
}

func TestABoxMayCrossTheAntimeridian(t *testing.T) {
	// Panning across the Pacific is a real thing, not an error.
	pacific, err := NewBounds(-20, 170, 20, -170)
	if err != nil {
		t.Fatalf("a Pacific viewport was rejected: %v", err)
	}
	if !pacific.CrossesAntimeridian() {
		t.Fatal("the Pacific box was not recognised as crossing; the query would return the whole world")
	}

	normal, _ := NewBounds(-20, -170, 20, 170)
	if normal.CrossesAntimeridian() {
		t.Error("a normal box was treated as crossing")
	}
}
