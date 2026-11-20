package kritiimages

import (
	"fmt"
	"image/color"
	"sort"
	"strings"
)

// RecipeFromOptions converts the existing single-step option map and
// DestinationImage into an ordered, versioned Recipe.
//
// The existing API receives an unordered map, so the relative order of
// adjustments is not defined by the caller. To make the conversion
// deterministic this helper uses a fixed, documented order:
//
//	background, resize (from width/height/fit), blur, brightness, contrast,
//	gamma, saturation, sharpen, rotate, flip, radius, format, quality
//
// width/height with a fit mode become a single resize step. When both
// dimensions are set without a fit mode, the existing default ("crop") is
// preserved. A single dimension becomes a width or height step. Values are kept
// as-is, so an out-of-range value still fails validation with the same
// parameter ranges the single-step API documents.
func RecipeFromOptions(options map[TransformationOption]string, dest *DestinationImage) (*Recipe, error) {
	if dest == nil {
		dest = &DestinationImage{}
	}

	steps := make([]RecipeStep, 0, len(options)+3)

	backgroundValue, hasBackground := options[Background]
	if strings.TrimSpace(backgroundValue) == "" && dest.BgColor != nil {
		// The route parses `background=` into DestinationImage.BgColor, so a
		// caller that already did that must still get the color into the
		// recipe. Fully transparent means "no background", matching the
		// single-step API's default.
		if _, _, _, a := dest.BgColor.RGBA(); a != 0 {
			backgroundValue = colorToHex(dest.BgColor)
			hasBackground = true
		}
	}
	if hasBackground && strings.TrimSpace(backgroundValue) != "" {
		steps = append(steps, RecipeStep{Op: OpBackground, Value: backgroundValue})
	}

	fitMode, hasFit := options[Fit]
	width := dest.Width
	height := dest.Height
	if v, ok := options[Width]; ok && strings.TrimSpace(v) != "" {
		w, err := strictInt(v, 1, maxRecipeDimension, "width")
		if err != nil {
			return nil, err
		}
		width = w
	}
	if v, ok := options[Height]; ok && strings.TrimSpace(v) != "" {
		h, err := strictInt(v, 1, maxRecipeDimension, "height")
		if err != nil {
			return nil, err
		}
		height = h
	}

	switch {
	case hasFit:
		mode := strings.TrimSpace(fitMode)
		if mode == "" {
			mode = "crop"
		}
		steps = append(steps, RecipeStep{
			Op:     OpResize,
			Mode:   mode,
			Width:  width,
			Height: height,
		})
	case width > 0 && height > 0:
		// The single-step API applies "crop" when both dimensions are set
		// without an explicit fit mode; preserve that behavior.
		steps = append(steps, RecipeStep{
			Op:     OpResize,
			Mode:   "crop",
			Width:  width,
			Height: height,
		})
	case width > 0:
		steps = append(steps, RecipeStep{Op: OpWidth, Value: fmt.Sprintf("%d", width)})
	case height > 0:
		steps = append(steps, RecipeStep{Op: OpHeight, Value: fmt.Sprintf("%d", height)})
	}

	type orderedOption struct {
		key TransformationOption
		op  string
	}
	// Deterministic order for the remaining adjustment operations.
	for _, entry := range []orderedOption{
		{Blur, OpBlur},
		{Brightness, OpBrightness},
		{Contrast, OpContrast},
		{Gamma, OpGamma},
		{Saturation, OpSaturation},
		{Sharpen, OpSharpen},
		{Rotate, OpRotate},
		{Flip, OpFlip},
		{BorderRadius, OpRadius},
	} {
		if value, ok := options[entry.key]; ok && strings.TrimSpace(value) != "" {
			steps = append(steps, RecipeStep{Op: entry.op, Value: value})
		}
	}

	format := dest.Format
	if value, ok := options[Format]; ok && strings.TrimSpace(value) != "" {
		format = value
	}
	if format != "" {
		steps = append(steps, RecipeStep{Op: OpFormat, Format: format})
	}

	quality := dest.Quality
	if value, ok := options[Quality]; ok && strings.TrimSpace(value) != "" {
		q, err := strictInt(value, 1, 100, "quality")
		if err != nil {
			return nil, err
		}
		quality = q
	}
	if quality > 0 {
		steps = append(steps, RecipeStep{Op: OpQuality, Quality: quality})
	}

	if len(steps) == 0 {
		return nil, fmt.Errorf("%w: %w: no transformations provided", ErrInvalidRecipe, ErrInvalidRecipeParameter)
	}

	recipe := NewRecipe(steps...)
	if err := recipe.Validate(); err != nil {
		return nil, err
	}
	return recipe, nil
}

// colorToHex renders a color as #RRGGBB, or #RRGGBBAA when it is not opaque.
// It gives recipes a stable textual representation for background colors that
// arrived as color.Color in the existing API.
func colorToHex(c color.Color) string {
	r, g, b, a := c.RGBA()
	hex := fmt.Sprintf("#%02x%02x%02x", uint8(r>>8), uint8(g>>8), uint8(b>>8))
	if a != 0xffff {
		hex += fmt.Sprintf("%02x", uint8(a>>8))
	}
	return hex
}

// SortedOptionNames returns the option names present in a map, sorted. It is a
// small diagnostic helper for callers that want to log what a recipe was
// built from.
func SortedOptionNames(options map[TransformationOption]string) []string {
	names := make([]string, 0, len(options))
	for opt := range options {
		names = append(names, optionName(opt))
	}
	sort.Strings(names)
	return names
}

func optionName(opt TransformationOption) string {
	switch opt {
	case Flip:
		return "flip"
	case Blur:
		return "blur"
	case Brightness:
		return "brightness"
	case Contrast:
		return "contrast"
	case Fit:
		return "fit"
	case Gamma:
		return "gamma"
	case Rotate:
		return "rotate"
	case Saturation:
		return "saturation"
	case Sharpen:
		return "sharpen"
	case Background:
		return "background"
	case Width:
		return "width"
	case Height:
		return "height"
	case Format:
		return "format"
	case Quality:
		return "quality"
	case BorderRadius:
		return "radius"
	default:
		return fmt.Sprintf("unknown(%d)", int(opt))
	}
}
