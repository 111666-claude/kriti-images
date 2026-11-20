package kritiimages

import (
	"errors"
	"image/color"
	"testing"
)

func TestRecipeFromOptions(t *testing.T) {
	t.Run("dimensions and fit become an ordered resize step", func(t *testing.T) {
		recipe, err := RecipeFromOptions(
			map[TransformationOption]string{
				Blur:   "3",
				Format: "webp",
			},
			&DestinationImage{
				Width:   120,
				Height:  80,
				Format:  "webp",
				Quality: 75,
				BgColor: color.White,
			},
		)
		if err != nil {
			t.Fatalf("RecipeFromOptions failed: %v", err)
		}
		if err := recipe.Validate(); err != nil {
			t.Fatalf("converted recipe is invalid: %v", err)
		}

		ops := make([]string, 0, len(recipe.Steps))
		for _, step := range recipe.Steps {
			ops = append(ops, step.Op)
		}
		want := []string{OpBackground, OpResize, OpBlur, OpFormat, OpQuality}
		if len(ops) != len(want) {
			t.Fatalf("ops = %v, want %v", ops, want)
		}
		for i := range want {
			if ops[i] != want[i] {
				t.Fatalf("ops = %v, want %v", ops, want)
			}
		}
		if recipe.Steps[0].Value != "#ffffff" {
			t.Fatalf("background from DestinationImage = %q, want #ffffff", recipe.Steps[0].Value)
		}

		// Conversion must be deterministic across map iteration order.
		for i := 0; i < 10; i++ {
			again, err := RecipeFromOptions(
				map[TransformationOption]string{Blur: "3", Format: "webp"},
				&DestinationImage{Width: 120, Height: 80, Format: "webp", Quality: 75, BgColor: color.White},
			)
			if err != nil {
				t.Fatal(err)
			}
			for j := range again.Steps {
				if again.Steps[j] != recipe.Steps[j] {
					t.Fatalf("non-deterministic conversion at step %d: %+v vs %+v",
						j, again.Steps[j], recipe.Steps[j])
				}
			}
		}
	})

	t.Run("explicit fit mode", func(t *testing.T) {
		recipe, err := RecipeFromOptions(
			map[TransformationOption]string{Fit: "pad", Rotate: "90"},
			&DestinationImage{Width: 100, Height: 50, BgColor: color.White},
		)
		if err != nil {
			t.Fatal(err)
		}
		if recipe.Steps[0].Op != OpBackground {
			t.Fatalf("first step = %q, want background (to feed pad)", recipe.Steps[0].Op)
		}
		if recipe.Steps[1].Op != OpResize || recipe.Steps[1].Mode != "pad" {
			t.Fatalf("second step = %+v, want pad resize", recipe.Steps[1])
		}
	})

	t.Run("no transformations is rejected", func(t *testing.T) {
		_, err := RecipeFromOptions(nil, &DestinationImage{})
		if !errors.Is(err, ErrInvalidRecipeParameter) {
			t.Fatalf("error = %v, want ErrInvalidRecipeParameter", err)
		}
	})

	t.Run("out-of-range option keeps the endpoint range", func(t *testing.T) {
		_, err := RecipeFromOptions(
			map[TransformationOption]string{Blur: "999"},
			&DestinationImage{},
		)
		if !errors.Is(err, ErrInvalidRecipeParameter) {
			t.Fatalf("error = %v, want ErrInvalidRecipeParameter", err)
		}
	})
}

func TestBackgroundAloneDoesNotRepaintPixels(t *testing.T) {
	// Matches the single-step API: `background` supplies the fill color for
	// later pad/rotate/crop operations; it does not repaint an existing image.
	recipe := NewRecipe(RecipeStep{Op: OpBackground, Value: "red"})
	src := testImage(4, 4)

	result, err := ApplyRecipeToImage(src, "png", recipe)
	if err != nil {
		t.Fatalf("ApplyRecipeToImage failed: %v", err)
	}
	if got, want := rgbaAt(result.Image, 0, 0), rgbaAt(src, 0, 0); got != want {
		t.Fatalf("pixel changed by background-only recipe: got %v, want %v", got, want)
	}
}

func TestBackgroundFeedsPad(t *testing.T) {
	recipe := NewRecipe(
		RecipeStep{Op: OpBackground, Value: "red"},
		RecipeStep{Op: OpResize, Mode: "pad", Width: 8, Height: 8},
	)
	result, err := ApplyRecipeToImage(testImage(4, 8), "png", recipe)
	if err != nil {
		t.Fatalf("ApplyRecipeToImage failed: %v", err)
	}
	if result.Width != 8 || result.Height != 8 {
		t.Fatalf("size = %dx%d, want 8x8", result.Width, result.Height)
	}
	// The source is 4x8 after fitting inside 8x8, so the left/right padding is
	// the background color.
	if got := rgbaAt(result.Image, 0, 4); got.R < 200 || got.G > 60 || got.B > 60 {
		t.Fatalf("pad background = %v, want red", got)
	}
}

func TestCropOutsideCurrentImageIsAnError(t *testing.T) {
	recipe := NewRecipe(
		RecipeStep{Op: OpCrop, Width: 2, Height: 2},
		RecipeStep{Op: OpCrop, Width: 4, Height: 4},
	)
	_, err := ApplyRecipeToImage(testImage(8, 8), "png", recipe)
	if !errors.Is(err, ErrCropOutOfBounds) {
		t.Fatalf("error = %v, want ErrCropOutOfBounds", err)
	}
	if code := RecipeErrorCode(err); code != "crop_out_of_bounds" {
		t.Fatalf("error code = %q, want crop_out_of_bounds", code)
	}
}

func TestAllSupportedOperationsExecute(t *testing.T) {
	recipe := NewRecipe(
		RecipeStep{Op: OpBackground, Value: "#ffffff"},
		RecipeStep{Op: OpWidth, Value: "6"},
		RecipeStep{Op: OpHeight, Value: "6"},
		RecipeStep{Op: OpResize, Mode: "cover", Width: 6, Height: 6},
		RecipeStep{Op: OpBlur, Value: "1"},
		RecipeStep{Op: OpBrightness, Value: "10"},
		RecipeStep{Op: OpContrast, Value: "10"},
		RecipeStep{Op: OpGamma, Value: "1.2"},
		RecipeStep{Op: OpSaturation, Value: "20"},
		RecipeStep{Op: OpSharpen, Value: "0.6"},
		RecipeStep{Op: OpRotate, Value: "90"},
		RecipeStep{Op: OpFlip, Value: "hv"},
		RecipeStep{Op: OpRadius, Value: "10%"},
		RecipeStep{Op: OpFormat, Format: "jpeg"},
		RecipeStep{Op: OpQuality, Quality: 70},
	)
	result, err := ApplyRecipeToImage(testImage(8, 6), "png", recipe)
	if err != nil {
		t.Fatalf("ApplyRecipeToImage failed: %v", err)
	}
	if result.Format != "jpeg" || result.Quality != 70 {
		t.Fatalf("format/quality = %s/%d, want jpeg/70", result.Format, result.Quality)
	}
	if len(result.Steps) != len(recipe.Steps) {
		t.Fatalf("reported %d steps, want %d", len(result.Steps), len(recipe.Steps))
	}
}

func TestRecipeTooManySteps(t *testing.T) {
	steps := make([]RecipeStep, MaxRecipeSteps+1)
	for i := range steps {
		steps[i] = RecipeStep{Op: OpBlur, Value: "1"}
	}
	err := ValidateRecipe(NewRecipe(steps...))
	if !errors.Is(err, ErrRecipeTooLarge) {
		t.Fatalf("error = %v, want ErrRecipeTooLarge", err)
	}
}
