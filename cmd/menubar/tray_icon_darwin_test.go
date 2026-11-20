package main

import (
	"bytes"
	"image/png"
	"testing"
)

func TestTrayTemplates(t *testing.T) {
	templates, err := loadTrayTemplates()
	if err != nil {
		t.Fatal(err)
	}
	source, err := png.Decode(bytes.NewReader(iconOn))
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name    string
		data    []byte
		opacity uint32
	}{
		{"active", templates.plain, 65535},
		{"stopped", templates.stopped, 166 * 257},
	} {
		t.Run(test.name, func(t *testing.T) {
			mask, err := png.Decode(bytes.NewReader(test.data))
			if err != nil {
				t.Fatal(err)
			}
			if mask.Bounds().Dx() != 44 || mask.Bounds().Dy() != 44 {
				t.Fatalf("unexpected mask dimensions: %v", mask.Bounds())
			}
			visible := 0
			for y := 0; y < 44; y++ {
				for x := 0; x < 44; x++ {
					red, green, blue, alpha := mask.At(x, y).RGBA()
					if red|green|blue != 0 {
						t.Fatal("template must contain only black and transparent pixels")
					}
					_, _, _, originalAlpha := source.At(x/2, y/2).RGBA()
					if originalAlpha == 0 && alpha != 0 {
						t.Fatal("template filled transparent robot background")
					}
					if alpha > 0 {
						visible++
					}
					if originalAlpha > 0 && alpha != test.opacity {
						t.Fatalf("opacity = %v, want %v", alpha, test.opacity)
					}
				}
			}
			if visible == 0 {
				t.Fatal("empty robot template")
			}
		})
	}
}
