// Package exifdate extracts EXIF capture/modify timestamps from image bytes.
// Fallback chain (same intent as the Python version): created is
// DateTimeOriginal, else CreateDate (DateTimeDigitized); modified is
// ModifyDate (DateTime), else created. Non-image files and images without
// EXIF yield (nil, nil) and callers fall back to last_modified/now.
package exifdate

import (
	"bytes"
	"time"

	"github.com/rwcarlsen/goexif/exif"
)

const exifLayout = "2006:01:02 15:04:05"

// Read returns (created, modified) or nils when unavailable. Timestamps are
// naive in EXIF; they are interpreted as UTC, matching the Python behavior of
// serializing them with a "Z" suffix.
func Read(raw []byte) (created, modified *time.Time) {
	defer func() {
		// goexif can panic on malformed input; treat that as "no EXIF".
		if r := recover(); r != nil {
			created, modified = nil, nil
		}
	}()
	x, err := exif.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, nil
	}
	get := func(field exif.FieldName) *time.Time {
		tag, err := x.Get(field)
		if err != nil {
			return nil
		}
		s, err := tag.StringVal()
		if err != nil {
			return nil
		}
		t, err := time.Parse(exifLayout, s)
		if err != nil {
			return nil
		}
		return &t
	}
	created = get(exif.DateTimeOriginal)
	if created == nil {
		created = get(exif.DateTimeDigitized)
	}
	modified = get(exif.DateTime)
	if modified == nil {
		modified = created
	}
	return created, modified
}
