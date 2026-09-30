package config

import (
	"strings"
	"testing"
)

func TestParseGeometry(t *testing.T) {
	tests := []struct {
		in      string
		want    Geometry
		wantErr bool
	}{
		{"960x540", Geometry{W: 960, H: 540}, false},
		{"1280x720+100+50", Geometry{W: 1280, H: 720, X: 100, Y: 50, HasPos: true}, false},
		{"640x360-20-30", Geometry{W: 640, H: 360, X: -20, Y: -30, HasPos: true}, false},
		{"640x360+0+0", Geometry{W: 640, H: 360, X: 0, Y: 0, HasPos: true}, false},
		{"  800x600  ", Geometry{W: 800, H: 600}, false},
		{"800x600+10-10", Geometry{W: 800, H: 600, X: 10, Y: -10, HasPos: true}, false},

		{"", Geometry{}, true},
		{"960", Geometry{}, true},
		{"960x", Geometry{}, true},
		{"x540", Geometry{}, true},
		{"960X540", Geometry{}, true},        // capital X is not accepted
		{"960x540+100", Geometry{}, true},    // half a position
		{"32x32", Geometry{}, true},          // below MinDim
		{"100000x100", Geometry{}, true},     // above MaxDim
		{"960x540 +10+10", Geometry{}, true}, // inner whitespace
		{"-960x540", Geometry{}, true},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got, err := ParseGeometry(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseGeometry(%q) succeeded with %+v, want error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseGeometry(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("ParseGeometry(%q) = %+v, want %+v", tc.in, got, tc.want)
			}
		})
	}
}

func TestValidateAddr(t *testing.T) {
	good := []string{"0.0.0.0:8787", "127.0.0.1:1", "172.60.3.42:65535", ":8080"}
	for _, a := range good {
		if err := ValidateAddr(a); err != nil {
			t.Errorf("ValidateAddr(%q) = %v, want nil", a, err)
		}
	}
	bad := []string{"", "8787", "0.0.0.0", "0.0.0.0:0", "0.0.0.0:70000",
		"0.0.0.0:abc", "not-an-ip:80", "999.1.1.1:80"}
	for _, a := range bad {
		if err := ValidateAddr(a); err == nil {
			t.Errorf("ValidateAddr(%q) = nil, want error", a)
		}
	}
}

func baseOptions() Options {
	return Options{
		Addr:      "0.0.0.0:8787",
		Geometry:  Geometry{W: 960, H: 540},
		Opacity:   235,
		FPS:       15,
		Quality:   70,
		ReadLimit: 1 << 20,
	}
}

func TestOptionsValidate(t *testing.T) {
	base := baseOptions()
	if err := base.Validate(); err != nil {
		t.Fatalf("baseline options rejected: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*Options)
		want   string
	}{
		{"bad addr", func(o *Options) { o.Addr = "nope" }, "address"},
		{"tiny overlay", func(o *Options) { o.Geometry.W = 10 }, "at least"},
		{"huge overlay", func(o *Options) { o.Geometry.H = 99999 }, "at most"},
		{"opacity zero", func(o *Options) { o.Opacity = 0 }, "opacity"},
		{"opacity over", func(o *Options) { o.Opacity = 256 }, "opacity"},
		{"fps zero", func(o *Options) { o.FPS = 0 }, "fps"},
		{"fps over", func(o *Options) { o.FPS = 61 }, "fps"},
		{"quality zero", func(o *Options) { o.Quality = 0 }, "quality"},
		{"quality over", func(o *Options) { o.Quality = 101 }, "quality"},
		{"read limit", func(o *Options) { o.ReadLimit = 0 }, "read limit"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			o := baseOptions()
			tc.mutate(&o)
			err := o.Validate()
			if err == nil {
				t.Fatalf("Validate() = nil, want error mentioning %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Validate() = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestClampOpacity(t *testing.T) {
	tests := []struct{ in, want int }{
		{-100, 8}, {0, 8}, {7, 8}, {8, 8},
		{128, 128}, {255, 255}, {256, 255}, {9999, 255},
	}
	for _, tc := range tests {
		if got := ClampOpacity(tc.in); got != tc.want {
			t.Errorf("ClampOpacity(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}
