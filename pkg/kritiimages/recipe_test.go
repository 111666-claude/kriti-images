package kritiimages

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mapSource is an in-memory ImageSource used by the tests. Paths are keys.
type mapSource struct {
	images  map[string]image.Image
	formats map[string]string
	err     error
}

func (m *mapSource) GetImage(_ context.Context, fileName string) (image.Image, string, error) {
	if m.err != nil {
		return nil, "", m.err
	}
	img, ok := m.images[fileName]
	if !ok {
		return nil, "", errors.New("not found")
	}
	return img, m.formats[fileName], nil
}

func (m *mapSource) UploadImage(context.Context, string, image.Image) error { return nil }

// testImage returns an asymmetric gradient so that changing the order of
// pixel operations changes the result and can be detected.
func testImage(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, color.RGBA{
				R: uint8(x * 30),
				G: uint8(y * 40),
				B: uint8((x*7 + y*13) % 256),
				A: 255,
			})
		}
	}
	return img
}

func newTestService(img image.Image, format string) *KritiImages {
	src := &mapSource{
		images:  map[string]image.Image{"cover.png": img},
		formats: map[string]string{"cover.png": format},
	}
	return New(map[string]ImageSource{"local": src}, src)
}

func rgbaAt(img image.Image, x, y int) color.RGBA {
	r, g, b, a := img.At(x, y).RGBA()
	return color.RGBA{uint8(r >> 8), uint8(g >> 8), uint8(b >> 8), uint8(a >> 8)}
}

// Fixed recipes, deliberately using the same three operations in different
// orders. Because the test image is asymmetric, both orders produce the same
// dimensions but different pixels.
var (
	recipeCropFlipResize = NewRecipe(
		RecipeStep{Op: OpCrop, X: 0, Y: 0, Width: 4, Height: 6},
		RecipeStep{Op: OpFlip, Value: "h"},
		RecipeStep{Op: OpResize, Width: 2, Mode: "contain"},
		RecipeStep{Op: OpFormat, Format: "webp"},
		RecipeStep{Op: OpQuality, Quality: 80},
	)

	recipeFlipCropResize = NewRecipe(
		RecipeStep{Op: OpFlip, Value: "h"},
		RecipeStep{Op: OpCrop, X: 0, Y: 0, Width: 4, Height: 6},
		RecipeStep{Op: OpResize, Width: 2, Height: 0, Mode: "contain"},
		RecipeStep{Op: OpFormat, Format: "png"},
		RecipeStep{Op: OpQuality, Value: "90"},
	)
)

func TestRecipeOrderIsRespected(t *testing.T) {
	src := testImage(8, 6)

	a, err := ApplyRecipeToImage(src, "png", recipeCropFlipResize)
	if err != nil {
		t.Fatalf("recipe A failed: %v", err)
	}
	b, err := ApplyRecipeToImage(src, "png", recipeFlipCropResize)
	if err != nil {
		t.Fatalf("recipe B failed: %v", err)
	}

	if a.Width != 2 || a.Height != 3 {
		t.Fatalf("recipe A size = %dx%d, want 2x3", a.Width, a.Height)
	}
	if b.Width != 2 || b.Height != 3 {
		t.Fatalf("recipe B size = %dx%d, want 2x3", b.Width, b.Height)
	}
	if a.Format != "webp" || a.Quality != 80 {
		t.Fatalf("recipe A output = %s/%d, want webp/80", a.Format, a.Quality)
	}
	if b.Format != "png" || b.Quality != 90 {
		t.Fatalf("recipe B output = %s/%d, want png/90", b.Format, b.Quality)
	}

	// Cropping the left half and then mirroring is not the same image as
	// mirroring and then cropping the left half of the mirrored image.
	if rgbaAt(a.Image, 0, 0) == rgbaAt(b.Image, 0, 0) {
		t.Fatalf("recipes with different step order produced identical pixels at (0,0): %v",
			rgbaAt(a.Image, 0, 0))
	}

	// The per-step report must describe the geometry after each step.
	wantA := []RecipeStepReport{
		{Index: 0, Op: "crop", Width: 4, Height: 6, Format: "png"},
		{Index: 1, Op: "flip", Width: 4, Height: 6, Format: "png"},
		{Index: 2, Op: "resize", Width: 2, Height: 3, Format: "png"},
		{Index: 3, Op: "format", Width: 2, Height: 3, Format: "webp"},
		{Index: 4, Op: "quality", Width: 2, Height: 3, Format: "webp"},
	}
	if len(a.Steps) != len(wantA) {
		t.Fatalf("recipe A reported %d steps, want %d", len(a.Steps), len(wantA))
	}
	for i := range wantA {
		if a.Steps[i] != wantA[i] {
			t.Errorf("recipe A step %d = %+v, want %+v", i, a.Steps[i], wantA[i])
		}
	}

	if b.Steps[0].Width != 8 || b.Steps[0].Height != 6 {
		t.Errorf("recipe B first step should report the flipped full image, got %dx%d",
			b.Steps[0].Width, b.Steps[0].Height)
	}
}

func TestSourceImageIsNotMutated(t *testing.T) {
	src := testImage(8, 6)
	before := make([]color.RGBA, 0, 8*6)
	for y := 0; y < 6; y++ {
		for x := 0; x < 8; x++ {
			before = append(before, rgbaAt(src, x, y))
		}
	}

	if _, err := ApplyRecipeToImage(src, "png", recipeCropFlipResize); err != nil {
		t.Fatalf("recipe failed: %v", err)
	}

	i := 0
	for y := 0; y < 6; y++ {
		for x := 0; x < 8; x++ {
			if got := rgbaAt(src, x, y); got != before[i] {
				t.Fatalf("source pixel (%d,%d) changed: got %v, want %v", x, y, got, before[i])
			}
			i++
		}
	}
}

func TestRecipeApplyEncodesAndReportsFormat(t *testing.T) {
	service := newTestService(testImage(8, 6), "png")

	result, err := service.ApplyRecipe(context.Background(), "cover.png", recipeCropFlipResize)
	if err != nil {
		t.Fatalf("ApplyRecipe failed: %v", err)
	}
	if result.Buffer == nil || result.Buffer.Len() == 0 {
		t.Fatal("expected encoded output")
	}
	if result.Format != "webp" || result.Quality != 80 {
		t.Fatalf("result = %s/%d, want webp/80", result.Format, result.Quality)
	}
	if result.Width != 2 || result.Height != 3 {
		t.Fatalf("result size = %dx%d, want 2x3", result.Width, result.Height)
	}
	if result.Bytes != result.Buffer.Len() {
		t.Fatalf("result bytes = %d, buffer length = %d", result.Bytes, result.Buffer.Len())
	}

	// A PNG recipe must decode as a 2x3 PNG.
	pngResult, err := service.ApplyRecipe(context.Background(), "cover.png", recipeFlipCropResize)
	if err != nil {
		t.Fatalf("ApplyRecipe failed: %v", err)
	}
	decoded, format, err := image.Decode(bytes.NewReader(pngResult.Buffer.Bytes()))
	if err != nil {
		t.Fatalf("failed to decode output: %v", err)
	}
	if format != "png" {
		t.Fatalf("encoded format = %q, want png", format)
	}
	if decoded.Bounds().Dx() != 2 || decoded.Bounds().Dy() != 3 {
		t.Fatalf("decoded size = %dx%d, want 2x3", decoded.Bounds().Dx(), decoded.Bounds().Dy())
	}
}

func TestRecipeJSONRoundTripCarriesVersion(t *testing.T) {
	// Version omitted on input: it is filled in with the current version.
	var recipe Recipe
	if err := json.Unmarshal([]byte(`{
		"steps": [
			{"operation": "blur", "value": 4},
			{"op": "width", "value": "120"},
			{"op": "format", "value": "jpeg"},
			{"op": "rotate", "value": "90"}
		]
	}`), &recipe); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if recipe.Version != RecipeVersion {
		t.Fatalf("version = %d, want %d", recipe.Version, RecipeVersion)
	}
	if err := recipe.Validate(); err != nil {
		t.Fatalf("recipe should be valid: %v", err)
	}
	if recipe.Steps[0].Value != "4" {
		t.Fatalf("numeric value not preserved: %q", recipe.Steps[0].Value)
	}

	encoded, err := json.Marshal(recipe)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	if !strings.Contains(string(encoded), `"version":1`) {
		t.Fatalf("marshalled recipe is missing version: %s", encoded)
	}

	parsed, err := ParseRecipe(encoded)
	if err != nil {
		t.Fatalf("ParseRecipe failed: %v", err)
	}
	if len(parsed.Steps) != 4 || parsed.Steps[2].Format != "" || parsed.Steps[2].Value != "jpeg" {
		t.Fatalf("round-trip lost data: %+v", parsed.Steps)
	}

	// A recipe built in Go and emitted by a struct literal must also carry a
	// version, so it can be stored and replayed later.
	encoded, err = json.Marshal(Recipe{Steps: []RecipeStep{{Op: OpBlur, Value: "2"}}})
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	if !strings.Contains(string(encoded), `"version":1`) {
		t.Fatalf("struct literal recipe is missing version: %s", encoded)
	}
}

func TestParseRecipeRejectsTypos(t *testing.T) {
	for _, tc := range []struct {
		name string
		data string
	}{
		{"unknown top-level field", `{"version":1,"stepz":[{"op":"blur","value":"2"}]}`},
		{"unknown step field", `{"version":1,"steps":[{"op":"crop","wdith":10,"width":10,"height":10}]}`},
		{"trailing data", `{"version":1,"steps":[{"op":"blur","value":"2"}]} {}`},
		{"wrong value type", `{"version":1,"steps":[{"op":"blur","value":{"a":1}}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseRecipe([]byte(tc.data)); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestRecipeValidationRejectsBadInputWithoutReadingImages(t *testing.T) {
	tests := []struct {
		name    string
		recipe  *Recipe
		wantErr error
		code    string
		stepIdx int
	}{
		{
			name:    "unsupported version",
			recipe:  &Recipe{Version: 2, Steps: []RecipeStep{{Op: OpBlur, Value: "2"}}},
			wantErr: ErrUnsupportedRecipeVersion,
			code:    "unsupported_version",
			stepIdx: -1,
		},
		{
			name:    "unknown operation",
			recipe:  NewRecipe(RecipeStep{Op: "explode", Value: "1"}),
			wantErr: ErrUnknownRecipeOperation,
			code:    "unknown_operation",
			stepIdx: 0,
		},
		{
			name:    "blur below range",
			recipe:  NewRecipe(RecipeStep{Op: OpBlur, Value: "0"}),
			wantErr: ErrInvalidRecipeParameter,
			code:    "invalid_parameter",
			stepIdx: 0,
		},
		{
			name:    "brightness above range",
			recipe:  NewRecipe(RecipeStep{Op: OpBrightness, Value: "101"}),
			wantErr: ErrInvalidRecipeParameter,
			code:    "invalid_parameter",
			stepIdx: 0,
		},
		{
			name:    "quality above range",
			recipe:  NewRecipe(RecipeStep{Op: OpQuality, Quality: 101}),
			wantErr: ErrInvalidRecipeParameter,
			code:    "invalid_parameter",
			stepIdx: 0,
		},
		{
			name:    "width below range",
			recipe:  NewRecipe(RecipeStep{Op: OpWidth, Width: 0, Value: "0"}),
			wantErr: ErrInvalidRecipeParameter,
			code:    "invalid_parameter",
			stepIdx: 0,
		},
		{
			name:    "unsupported format",
			recipe:  NewRecipe(RecipeStep{Op: OpFormat, Format: "gif"}),
			wantErr: ErrInvalidImageFormat,
			code:    "unsupported_format",
			stepIdx: 0,
		},
		{
			name:    "fit without dimensions",
			recipe:  NewRecipe(RecipeStep{Op: OpFit, Value: "contain"}),
			wantErr: ErrInvalidRecipeParameter,
			code:    "invalid_parameter",
			stepIdx: 0,
		},
		{
			name:    "empty recipe",
			recipe:  NewRecipe(),
			wantErr: ErrInvalidRecipeParameter,
			code:    "invalid_parameter",
			stepIdx: -1,
		},
	}

	// A source that would panic if consulted proves validation is static.
	service := New(
		map[string]ImageSource{"local": &panicSource{}},
		&panicSource{},
	)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateRecipe(tt.recipe)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("ValidateRecipe error = %v, want %v", err, tt.wantErr)
			}
			if !errors.Is(err, ErrInvalidRecipe) {
				t.Fatalf("error should also be ErrInvalidRecipe: %v", err)
			}
			if got := RecipeErrorCode(err); got != tt.code {
				t.Errorf("error code = %q, want %q", got, tt.code)
			}
			if tt.stepIdx >= 0 {
				var stepErr *RecipeStepError
				if !errors.As(err, &stepErr) {
					t.Fatalf("expected RecipeStepError, got %T", err)
				}
				if stepErr.Index != tt.stepIdx {
					t.Errorf("step index = %d, want %d", stepErr.Index, tt.stepIdx)
				}
			}

			// Applying an invalid recipe must fail before touching the source.
			if _, err := service.ApplyRecipe(context.Background(), "cover.png", tt.recipe); !errors.Is(err, tt.wantErr) {
				t.Fatalf("ApplyRecipe error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

type panicSource struct{}

func (p *panicSource) GetImage(context.Context, string) (image.Image, string, error) {
	panic("GetImage must not be called for an invalid recipe")
}

func (p *panicSource) UploadImage(context.Context, string, image.Image) error {
	panic("UploadImage must not be called")
}

func TestRepeatedCropThatEmptiesImageFails(t *testing.T) {
	// First crop removes most of the image; the second crop targets a region
	// that no longer exists. This must be reported, not silently returned as a
	// zero-sized image.
	recipe := NewRecipe(
		RecipeStep{Op: OpCrop, Width: 4, Height: 4},
		RecipeStep{Op: OpCrop, X: 100, Y: 100, Width: 4, Height: 4},
	)
	if err := recipe.Validate(); err != nil {
		t.Fatalf("recipe is statically valid: %v", err)
	}

	result, err := ApplyRecipeToImage(testImage(8, 6), "png", recipe)
	if !errors.Is(err, ErrEmptyRecipeResult) {
		t.Fatalf("error = %v, want ErrEmptyRecipeResult", err)
	}
	if result != nil {
		t.Fatal("expected nil result for an empty image")
	}
	if code := RecipeErrorCode(err); code != "empty_image" {
		t.Fatalf("error code = %q, want empty_image", code)
	}

	var stepErr *RecipeStepError
	if !errors.As(err, &stepErr) || stepErr.Index != 1 {
		t.Fatalf("expected failure at step 1, got %v", err)
	}

	// The same recipe through the full apply path yields no encoded output.
	service := newTestService(testImage(8, 6), "png")
	encoded, err := service.ApplyRecipe(context.Background(), "cover.png", recipe)
	if err == nil || encoded != nil {
		t.Fatalf("expected nil result and error, got %v / %v", encoded, err)
	}
}

func TestDryRunReportsFailureBeforeEncoding(t *testing.T) {
	recipe := NewRecipe(
		RecipeStep{Op: OpCrop, Width: 4, Height: 4},
		RecipeStep{Op: OpCrop, X: 50, Y: 50, Width: 4, Height: 4},
		RecipeStep{Op: OpFormat, Format: "jpeg"},
	)
	_, err := DryRunRecipe(testImage(8, 6), "png", recipe)
	if !errors.Is(err, ErrEmptyRecipeResult) {
		t.Fatalf("DryRunRecipe error = %v, want ErrEmptyRecipeResult", err)
	}
}

func TestBatchLimitsAndPerImageOutcomes(t *testing.T) {
	service := newTestService(testImage(8, 6), "png")

	t.Run("too many images is rejected before reading any image", func(t *testing.T) {
		paths := make([]string, MaxRecipeBatchSize+1)
		for i := range paths {
			paths[i] = "missing.png"
		}
		items, err := service.ApplyRecipeBatch(context.Background(), paths, recipeFlipCropResize)
		if !errors.Is(err, ErrBatchTooLarge) {
			t.Fatalf("error = %v, want ErrBatchTooLarge", err)
		}
		if items != nil {
			t.Fatal("expected no items for a rejected batch")
		}
		if code := RecipeErrorCode(err); code != "batch_too_large" {
			t.Fatalf("error code = %q, want batch_too_large", code)
		}
	})

	t.Run("empty batch is rejected", func(t *testing.T) {
		if _, err := service.ApplyRecipeBatch(context.Background(), nil, recipeFlipCropResize); !errors.Is(err, ErrEmptyBatch) {
			t.Fatalf("error = %v, want ErrEmptyBatch", err)
		}
	})

	t.Run("missing images are reported per item", func(t *testing.T) {
		items, err := service.ApplyRecipeBatch(context.Background(),
			[]string{"cover.png", "missing.png"}, recipeFlipCropResize)
		if err != nil {
			t.Fatalf("unexpected batch error: %v", err)
		}
		if len(items) != 2 {
			t.Fatalf("got %d items, want 2", len(items))
		}
		if items[0].Result == nil || items[0].Error != "" {
			t.Fatalf("first item should succeed: %+v", items[0])
		}
		if items[1].Result != nil {
			t.Fatal("failed item must not carry a result")
		}
		if items[1].ErrorCode != "source_not_found" {
			t.Fatalf("error code = %q, want source_not_found", items[1].ErrorCode)
		}
	})
}

func TestWriteRecipeFilesFailureLeavesNoPartialOutput(t *testing.T) {
	dir := t.TempDir()
	service := newTestService(testImage(8, 6), "png")

	t.Run("missing source writes nothing", func(t *testing.T) {
		outDir := filepath.Join(dir, "missing-source")
		_, err := service.WriteRecipeFiles(context.Background(),
			[]string{"cover.png", "missing.png"},
			[]string{filepath.Join(outDir, "a.png"), filepath.Join(outDir, "b.png")},
			recipeFlipCropResize)
		if !errors.Is(err, ErrSourceImageNotFound) {
			t.Fatalf("error = %v, want ErrSourceImageNotFound", err)
		}
		assertEmptyDir(t, outDir)
	})

	t.Run("invalid recipe writes nothing", func(t *testing.T) {
		outDir := filepath.Join(dir, "invalid-recipe")
		_, err := service.WriteRecipeFiles(context.Background(),
			[]string{"cover.png"},
			[]string{filepath.Join(outDir, "a.png")},
			NewRecipe(RecipeStep{Op: OpBlur, Value: "0"}))
		if !errors.Is(err, ErrInvalidRecipe) {
			t.Fatalf("error = %v, want ErrInvalidRecipe", err)
		}
		if _, statErr := os.Stat(outDir); !os.IsNotExist(statErr) {
			t.Fatalf("output directory should not have been created, stat err = %v", statErr)
		}
	})

	t.Run("failure while staging removes all temporary files", func(t *testing.T) {
		outDir := filepath.Join(dir, "staging")
		if err := os.MkdirAll(outDir, 0o755); err != nil {
			t.Fatal(err)
		}
		// Make the second destination impossible to create: its parent is a
		// regular file, so MkdirAll fails after the first temp file exists.
		blocker := filepath.Join(outDir, "blocker")
		if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}

		_, err := service.WriteRecipeFiles(context.Background(),
			[]string{"cover.png", "cover.png"},
			[]string{filepath.Join(outDir, "a.png"), filepath.Join(blocker, "b.png")},
			recipeFlipCropResize)
		if !errors.Is(err, ErrRecipeWriteFailed) {
			t.Fatalf("error = %v, want ErrRecipeWriteFailed", err)
		}

		entries, readErr := os.ReadDir(outDir)
		if readErr != nil {
			t.Fatal(readErr)
		}
		for _, entry := range entries {
			if entry.Name() == "blocker" {
				continue
			}
			t.Errorf("unexpected leftover %q after failed write", entry.Name())
		}
		if _, statErr := os.Stat(filepath.Join(outDir, "a.png")); !os.IsNotExist(statErr) {
			t.Errorf("a.png should not exist after failed batch, stat err = %v", statErr)
		}
	})

	t.Run("duplicate destinations are rejected", func(t *testing.T) {
		dst := filepath.Join(dir, "dup", "a.png")
		_, err := service.WriteRecipeFiles(context.Background(),
			[]string{"cover.png", "cover.png"},
			[]string{dst, dst},
			recipeFlipCropResize)
		if !errors.Is(err, ErrInvalidRecipeParameter) {
			t.Fatalf("error = %v, want ErrInvalidRecipeParameter", err)
		}
		if _, statErr := os.Stat(dst); !os.IsNotExist(statErr) {
			t.Errorf("destination should not exist, stat err = %v", statErr)
		}
	})
}

func TestWriteRecipeFilesSuccess(t *testing.T) {
	dir := t.TempDir()
	service := newTestService(testImage(8, 6), "png")

	src := filepath.Join(dir, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	// Write a real PNG so the local file source can read it back.
	fileSource := &mapSource{
		images:  map[string]image.Image{"cover.png": testImage(8, 6)},
		formats: map[string]string{"cover.png": "png"},
	}
	service = New(map[string]ImageSource{"local": fileSource}, fileSource)

	out1 := filepath.Join(dir, "out", "a.png")
	out2 := filepath.Join(dir, "out", "b.png")
	items, err := service.WriteRecipeFiles(context.Background(),
		[]string{"cover.png", "cover.png"}, []string{out1, out2}, recipeFlipCropResize)
	if err != nil {
		t.Fatalf("WriteRecipeFiles failed: %v", err)
	}
	if len(items) != 2 || items[0].Result == nil || items[1].Result == nil {
		t.Fatalf("unexpected items: %+v", items)
	}

	for _, path := range []string{out1, out2} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("output %q not written: %v", path, err)
		}
		img, format, err := image.Decode(bytes.NewReader(data))
		if err != nil {
			t.Fatalf("output %q is not a valid image: %v", path, err)
		}
		if format != "png" || img.Bounds().Dx() != 2 || img.Bounds().Dy() != 3 {
			t.Fatalf("output %q = %s %dx%d, want png 2x3", path, format,
				img.Bounds().Dx(), img.Bounds().Dy())
		}
	}

	// No temporary staging files should remain.
	entries, err := os.ReadDir(filepath.Join(dir, "out"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected exactly 2 files, got %d", len(entries))
	}
}

func TestPreviewRecipeMatchesApplyImage(t *testing.T) {
	service := newTestService(testImage(8, 6), "png")
	preview, err := service.PreviewRecipe(context.Background(), "cover.png", recipeCropFlipResize)
	if err != nil {
		t.Fatalf("PreviewRecipe failed: %v", err)
	}
	applied, err := ApplyRecipeToImage(testImage(8, 6), "png", recipeCropFlipResize)
	if err != nil {
		t.Fatalf("ApplyRecipeToImage failed: %v", err)
	}
	if preview.Width != applied.Width || preview.Height != applied.Height ||
		preview.Format != applied.Format || len(preview.Steps) != len(applied.Steps) {
		t.Fatalf("preview %+v does not match apply %+v", preview, applied)
	}
}

// TestSingleStepTransformUnchanged guards the existing API: the default
// transformation path must still produce a plain re-encode of the source.
func TestSingleStepTransformUnchanged(t *testing.T) {
	src := testImage(8, 6)
	service := newTestService(src, "png")
	dest := &DestinationImage{BgColor: color.Transparent}

	out, err := service.Transform(context.Background(), "cover.png", dest, nil)
	if err != nil {
		t.Fatalf("Transform failed: %v", err)
	}
	if dest.Width != 8 || dest.Height != 6 || dest.Format != "png" || dest.Quality != 100 {
		t.Fatalf("defaults changed: %+v", dest)
	}

	want, err := service.formatTo(src, "png", 100)
	if err != nil {
		t.Fatalf("formatTo failed: %v", err)
	}
	if !bytes.Equal(out.Bytes(), want.Bytes()) {
		t.Fatal("Transform without options no longer matches a plain re-encode")
	}

	// A single explicit option still works through the original map API.
	// Dimensions live on DestinationImage in the existing API; with no
	// explicit fit mode the default "crop" behavior applies.
	dest2 := &DestinationImage{BgColor: color.Transparent, Width: 4, Height: 3, Format: "png"}
	out2, err := service.Transform(context.Background(), "cover.png", dest2, nil)
	if err != nil {
		t.Fatalf("Transform with options failed: %v", err)
	}
	img, _, err := image.Decode(bytes.NewReader(out2.Bytes()))
	if err != nil {
		t.Fatalf("failed to decode: %v", err)
	}
	if img.Bounds().Dx() != 4 || img.Bounds().Dy() != 3 {
		t.Fatalf("size = %dx%d, want 4x3", img.Bounds().Dx(), img.Bounds().Dy())
	}
}

func assertEmptyDir(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("directory %q is not empty: %v", dir, names)
	}
}
