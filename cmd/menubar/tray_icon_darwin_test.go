package main

import (
	"bytes"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
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
		badge   bool
	}{
		{"starting", templates.plain, 65535, false},
		{"stopped", templates.stopped, 166 * 257, false},
		{"running", templates.running, 65535, true},
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
					if !test.badge && originalAlpha > 0 && alpha != test.opacity {
						t.Fatalf("opacity = %v, want %v", alpha, test.opacity)
					}
					pointX, pointY := (float64(x)+0.5)*20/44, (float64(y)+0.5)*20/44
					if test.badge && (pointX-15)*(pointX-15)+(pointY-15)*(pointY-15) < 5*5 && alpha != 0 {
						t.Fatal("robot overlaps badge clearance")
					}
				}
			}
			if visible == 0 {
				t.Fatal("empty robot template")
			}
		})
	}
}

func TestNativeTrayIcon(t *testing.T) {
	directory := t.TempDir()
	// optionally retain native renders for local visual inspection.
	if artifacts := os.Getenv("VEKIL_TRAY_TEST_ARTIFACTS"); artifacts != "" {
		directory = artifacts
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	templates, err := loadTrayTemplates()
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{"plain": templates.plain, "running": templates.running, "stopped": templates.stopped} {
		if err := os.WriteFile(filepath.Join(directory, name+".png"), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	executable := filepath.Join(directory, "tray-icon-test")
	build := exec.CommandContext(t.Context(), "xcrun", "clang", "-fobjc-arc", "-framework", "Cocoa", "-Wall", "-Werror", "-o", executable, "testdata/tray_icon_test.m")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("compile native tray tests: %v\n%s", err, output)
	}
	run := exec.CommandContext(t.Context(), executable, directory)
	if output, err := run.CombinedOutput(); err != nil {
		t.Fatalf("native tray tests: %v\n%s", err, output)
	}
}
