package handler

import (
	"math"
	"strconv"
	"strings"
)

// defaultBookingAccent is users.booking_accent's column default (migration 00067): the
// value every host has until they pick one.
const defaultBookingAccent = "#111827"

// chosenAccent returns the host's accent for the hosted booking and manage pages, or ""
// when they never chose one. The pages set --booking-accent only for a non-empty value,
// so an unchosen accent leaves the page theme's own primary in charge. Without this the
// column default, which every row carries, would override this fork's District palette
// on every tenant's pages with near-black.
func chosenAccent(color string) string {
	if strings.EqualFold(color, defaultBookingAccent) {
		return ""
	}
	return color
}

func accentForeground(color string) string {
	if len(color) != 7 {
		return "#ffffff"
	}
	value, err := strconv.ParseUint(color[1:], 16, 32)
	if err != nil {
		return "#ffffff"
	}
	linear := func(v uint64) float64 {
		c := float64(v) / 255
		if c <= 0.04045 {
			return c / 12.92
		}
		return math.Pow((c+0.055)/1.055, 2.4)
	}
	luminance := 0.2126*linear(value>>16) + 0.7152*linear((value>>8)&255) + 0.0722*linear(value&255)
	if luminance > 0.179 {
		return "#000000"
	}
	return "#ffffff"
}
