package kritiimages

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/disintegration/gift"
	"github.com/kritihq/kriti-images/internal/transformations"
	"github.com/kritihq/kriti-images/internal/utils"
)

// RecipeVersion is the only recipe schema version understood by this build.
//
// A recipe is deliberately versioned: recipes are meant to be stored and
// reused, so a future change to the step vocabulary or semantics must bump
// this number instead of silently changing the meaning of existing recipes.
const RecipeVersion = 1

const (
	// MaxRecipeSteps bounds the amount of work a single recipe may describe.
	// It protects the service from pathological recipes while still leaving
	// ample room for realistic editing pipelines.
	MaxRecipeSteps = 64

	// MaxRecipeBatchSize bounds the number of images a single batch call may
	// request. Larger batches are rejected up-front, before any image is read.
	MaxRecipeBatchSize = 100

	// maxRecipeDimension mirrors the 1..10000 range accepted by the URL API.
	maxRecipeDimension = 10000
)

// Recipe operation names. They intentionally use the same names as the URL
// transformation options so a recipe can be written by someone already
// familiar with /cgi/images/tr:....
const (
	OpFlip       = "flip"
	OpBlur       = "blur"
	OpBrightness = "brightness"
	OpContrast   = "contrast"
	OpFit        = "fit"
	OpGamma      = "gamma"
	OpRotate     = "rotate"
	OpSaturation = "saturation"
	OpSharpen    = "sharpen"
	OpBackground = "background"
	OpWidth      = "width"
	OpHeight     = "height"
	OpFormat     = "format"
	OpQuality    = "quality"
	OpRadius     = "radius"

	// OpResize and OpCrop are the ordered, self-contained equivalents of the
	// dimension options above. They are the recommended vocabulary for new
	// recipes because they state both the geometry and the fitting mode in a
	// single step.
	OpResize = "resize"
	OpCrop   = "crop"
)

// Recipe validation and execution errors. They are sentinels so callers (and
// the HTTP layer) can turn a failure into a stable, machine-readable outcome.
var (
	ErrInvalidRecipe            = errors.New("invalid recipe")
	ErrUnsupportedRecipeVersion = errors.New("unsupported recipe version")
	ErrUnknownRecipeOperation   = errors.New("unknown recipe operation")
	ErrInvalidRecipeParameter   = errors.New("invalid recipe parameter")
	ErrEmptyRecipeResult        = errors.New("recipe produced an empty image")
	ErrCropOutOfBounds          = errors.New("crop region is outside the current image")
	ErrRecipeTooLarge           = errors.New("recipe has too many steps")
	ErrBatchTooLarge            = errors.New("batch has too many images")
	ErrEmptyBatch               = errors.New("batch has no images")
	ErrRecipeWriteFailed        = errors.New("failed to write recipe output")
)

// Recipe is a versioned, ordered list of transformations that can be stored,
// validated and replayed against many source images.
type Recipe struct {
	Version int          `json:"version"`
	Steps   []RecipeStep `json:"steps"`
}

// RecipeStep is one ordered transformation in a Recipe.
//
// Which fields are used depends on Op:
//
//	flip, blur, brightness, contrast, gamma, rotate, saturation,
//	sharpen, background, radius  -> Value
//	format                       -> Format (or Value)
//	quality                      -> Quality (or Value)
//	width, height                -> Width / Height (or Value)
//	resize, fit                  -> Width, Height and Mode (or Value as mode)
//	crop                         -> X, Y, Width and Height
//
// Op is matched case-insensitively. Dimension/quality fields may also be
// supplied through Value so recipes can mirror the URL option syntax.
type RecipeStep struct {
	Op      string `json:"op"`
	Value   string `json:"value,omitempty"`
	Width   int    `json:"width,omitempty"`
	Height  int    `json:"height,omitempty"`
	X       int    `json:"x,omitempty"`
	Y       int    `json:"y,omitempty"`
	Mode    string `json:"mode,omitempty"`
	Quality int    `json:"quality,omitempty"`
	Format  string `json:"format,omitempty"`
}

// RecipeStepError identifies the step that made a recipe invalid or failed
// during execution. Index is zero-based.
type RecipeStepError struct {
	Index int
	Op    string
	Err   error
}

func (e *RecipeStepError) Error() string {
	return fmt.Sprintf("recipe step %d (%s): %v", e.Index, e.Op, e.Err)
}

func (e *RecipeStepError) Unwrap() error { return e.Err }

// RecipeStepReport describes the image geometry and output format immediately
// after one step completed.
type RecipeStepReport struct {
	Index  int    `json:"index"`
	Op     string `json:"op"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
	Format string `json:"format"`
}

// RecipeImageResult is the in-memory result of executing a recipe. No encoding
// has happened yet.
type RecipeImageResult struct {
	Image   image.Image        `json:"-"`
	Steps   []RecipeStepReport `json:"steps"`
	Width   int                `json:"width"`
	Height  int                `json:"height"`
	Format  string             `json:"format"`
	Quality int                `json:"quality"`
}

// RecipeResult is a fully encoded recipe output. On any error, callers receive
// a nil result: a partially transformed image is never returned or written.
type RecipeResult struct {
	Buffer  *bytes.Buffer      `json:"-"`
	Steps   []RecipeStepReport `json:"steps"`
	Width   int                `json:"width"`
	Height  int                `json:"height"`
	Format  string             `json:"format"`
	Quality int                `json:"quality"`
	Bytes   int                `json:"bytes"`
}

// RecipeBatchItem is the outcome of applying a recipe to one image in a batch.
// A failed item never carries an encoded image.
type RecipeBatchItem struct {
	Index     int           `json:"index"`
	Path      string        `json:"path"`
	Result    *RecipeResult `json:"result,omitempty"`
	Error     string        `json:"error,omitempty"`
	ErrorCode string        `json:"error_code,omitempty"`
}

// NewRecipe returns a version-1 recipe containing the supplied ordered steps.
func NewRecipe(steps ...RecipeStep) *Recipe {
	return &Recipe{Version: RecipeVersion, Steps: append([]RecipeStep(nil), steps...)}
}

// MarshalJSON is implemented alongside UnmarshalJSON so encoded recipes always
// carry the schema version, even when the caller built the Recipe literal
// without setting Version.
func (r Recipe) MarshalJSON() ([]byte, error) {
	version := r.Version
	if version == 0 {
		version = RecipeVersion
	}
	steps := r.Steps
	if steps == nil {
		steps = []RecipeStep{}
	}
	return json.Marshal(struct {
		Version int          `json:"version"`
		Steps   []RecipeStep `json:"steps"`
	}{Version: version, Steps: steps})
}

// UnmarshalJSON applies the current schema version when the version field is
// omitted. That keeps hand-written recipes and recipes stored before the
// version field existed loadable. An explicitly unsupported version is still
// rejected by Validate, not silently upgraded.
func (r *Recipe) UnmarshalJSON(data []byte) error {
	var raw struct {
		Version *int         `json:"version"`
		Steps   []RecipeStep `json:"steps"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if err := rejectUnknownFields(data, recipeFieldsSet); err != nil {
		return err
	}
	r.Steps = raw.Steps
	if raw.Version == nil {
		r.Version = RecipeVersion
	} else {
		r.Version = *raw.Version
	}
	return nil
}

var recipeFieldsSet = map[string]struct{}{
	"version": {}, "steps": {},
}

// UnmarshalJSON accepts both "op" and "operation" for the step name and
// accepts a string, number or bool for Value.
func (s *RecipeStep) UnmarshalJSON(data []byte) error {
	var raw struct {
		Op        string          `json:"op"`
		Operation string          `json:"operation"`
		Value     json.RawMessage `json:"value"`
		Width     int             `json:"width"`
		Height    int             `json:"height"`
		X         int             `json:"x"`
		Y         int             `json:"y"`
		Mode      string          `json:"mode"`
		Quality   int             `json:"quality"`
		Format    string          `json:"format"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	*s = RecipeStep{
		Op:      firstNonEmpty(raw.Op, raw.Operation),
		Width:   raw.Width,
		Height:  raw.Height,
		X:       raw.X,
		Y:       raw.Y,
		Mode:    raw.Mode,
		Quality: raw.Quality,
		Format:  raw.Format,
	}

	// Reject misspelled/malformed numeric fields instead of treating them as
	// zero, which would otherwise silently drop part of a step.
	if err := rejectUnknownFields(data, recipeStepFields); err != nil {
		return err
	}

	if len(raw.Value) == 0 || string(raw.Value) == "null" {
		return nil
	}

	var str string
	if err := json.Unmarshal(raw.Value, &str); err == nil {
		s.Value = str
		return nil
	}

	// Numeric and boolean values are accepted for ergonomics, e.g.
	// {"op":"brightness","value":20}.
	var num json.Number
	if err := json.Unmarshal(raw.Value, &num); err == nil {
		s.Value = num.String()
		return nil
	}

	var boolean bool
	if err := json.Unmarshal(raw.Value, &boolean); err == nil {
		s.Value = strconv.FormatBool(boolean)
		return nil
	}

	return fmt.Errorf("value must be a string, number or bool")
}

// ParseRecipe decodes and validates a JSON recipe. This is the recommended
// entry point for recipes loaded from disk or a database.
func ParseRecipe(data []byte) (*Recipe, error) {
	var r Recipe
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&r); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidRecipe, err)
	}
	if dec.More() {
		return nil, fmt.Errorf("%w: unexpected trailing data after recipe", ErrInvalidRecipe)
	}
	if err := r.Validate(); err != nil {
		return nil, err
	}
	return &r, nil
}

// Validate checks the recipe without reading an image or writing any output.
// It verifies the schema version, step count, operation names and every static
// parameter (ranges, formats, modes, colors). Image-dependent failures such as
// an empty result are reported by DryRunRecipe or ApplyRecipeToImage.
func (r *Recipe) Validate() error {
	return ValidateRecipe(r)
}

// ValidateRecipe is the package-level form of Recipe.Validate.
func ValidateRecipe(r *Recipe) error {
	if r == nil {
		return fmt.Errorf("%w: recipe is nil", ErrInvalidRecipe)
	}
	if r.Version != RecipeVersion && r.Version != 0 {
		// Version 0 is accepted as an alias for the current version, matching
		// MarshalJSON, so a Recipe literal built without an explicit version
		// still validates. Any other version is from a different schema and
		// must not be interpreted with today's semantics.
		return fmt.Errorf("%w: %w: got version %d, want %d",
			ErrInvalidRecipe, ErrUnsupportedRecipeVersion, r.Version, RecipeVersion)
	}
	if len(r.Steps) == 0 {
		return fmt.Errorf("%w: %w: recipe must contain at least one step",
			ErrInvalidRecipe, ErrInvalidRecipeParameter)
	}
	if len(r.Steps) > MaxRecipeSteps {
		return fmt.Errorf("%w: %w: got %d steps, max %d",
			ErrInvalidRecipe, ErrRecipeTooLarge, len(r.Steps), MaxRecipeSteps)
	}

	for i := range r.Steps {
		if err := r.Steps[i].validate(); err != nil {
			return &RecipeStepError{Index: i, Op: r.Steps[i].Op, Err: err}
		}
	}
	return nil
}

func (s *RecipeStep) validate() error {
	op := normalizeOp(s.Op)
	switch op {
	case OpFlip:
		v := strings.ToLower(strings.TrimSpace(s.Value))
		if v != "h" && v != "v" && v != "hv" && v != "vh" {
			return invalidParam("flip", "value must be one of h, v, hv", s.Value)
		}

	case OpBlur:
		if _, err := strictFloat(s.Value, 1, 250, "blur"); err != nil {
			return err
		}

	case OpBrightness:
		if _, err := strictFloat(s.Value, -100, 100, "brightness"); err != nil {
			return err
		}

	case OpContrast:
		if _, err := strictFloat(s.Value, -100, 100, "contrast"); err != nil {
			return err
		}

	case OpGamma:
		if _, err := strictFloat(s.Value, 0.1, 2.0, "gamma"); err != nil {
			return err
		}

	case OpSaturation:
		if _, err := strictFloat(s.Value, -100, 500, "saturation"); err != nil {
			return err
		}

	case OpSharpen:
		if _, err := strictFloat(s.Value, 0.5, 1.5, "sharpen"); err != nil {
			return err
		}

	case OpRotate:
		if strings.TrimSpace(s.Value) == "" {
			return invalidParam("rotate", "value is required", s.Value)
		}
		if _, err := utils.ParseRotateAngle(s.Value); err != nil {
			return fmt.Errorf("%w: %w", ErrInvalidRecipe, err)
		}

	case OpBackground:
		if strings.TrimSpace(s.Value) == "" {
			return invalidParam("background", "value is required", s.Value)
		}
		if _, err := utils.ParseBackgroundColor(s.Value); err != nil {
			return fmt.Errorf("%w: %w: %v", ErrInvalidRecipe, ErrInvalidRecipeParameter, err)
		}

	case OpRadius:
		if _, err := transformations.CreateBorderRadiusFilter(s.Value); err != nil {
			return fmt.Errorf("%w: %w: %v", ErrInvalidRecipe, ErrInvalidRecipeParameter, err)
		}

	case OpFormat:
		value := firstNonEmpty(s.Format, s.Value)
		if value == "" {
			return invalidParam("format", "value or format field is required", value)
		}
		if _, err := utils.ParseFormatValue(value); err != nil {
			return fmt.Errorf("%w: %w: %w", ErrInvalidRecipe, ErrInvalidRecipeParameter, ErrInvalidImageFormat)
		}

	case OpQuality:
		value := s.Value
		if s.Quality > 0 {
			value = strconv.Itoa(s.Quality)
		}
		if _, err := strictInt(value, 1, 100, "quality"); err != nil {
			return err
		}

	case OpWidth, OpHeight:
		var value string
		if op == OpWidth && s.Width > 0 {
			value = strconv.Itoa(s.Width)
		} else if op == OpHeight && s.Height > 0 {
			value = strconv.Itoa(s.Height)
		} else {
			value = s.Value
		}
		if _, err := strictInt(value, 1, maxRecipeDimension, op); err != nil {
			return err
		}
		if op == OpWidth && s.Height != 0 {
			return invalidParam(op, "width step must not set height", "")
		}
		if op == OpHeight && s.Width != 0 {
			return invalidParam(op, "height step must not set width", "")
		}

	case OpResize, OpFit:
		mode := strings.ToLower(strings.TrimSpace(s.Mode))
		if mode == "" {
			mode = strings.ToLower(strings.TrimSpace(s.Value))
		}
		if mode == "" {
			mode = "contain"
		}
		if err := validateFitMode(mode, s.Width, s.Height); err != nil {
			return err
		}

	case OpCrop:
		if _, err := strictInt(strconv.Itoa(s.Width), 1, maxRecipeDimension, "crop width"); err != nil {
			return err
		}
		if _, err := strictInt(strconv.Itoa(s.Height), 1, maxRecipeDimension, "crop height"); err != nil {
			return err
		}
		// X and Y are offsets into the current image. They may point outside
		// the image; that is an image-dependent condition reported during
		// execution (as ErrEmptyRecipeResult), not a static parameter error.
		if s.X < 0 || s.Y < 0 {
			return invalidParam("crop", "x and y must not be negative", "")
		}

	default:
		return fmt.Errorf("%w: %w: unknown operation %q",
			ErrInvalidRecipe, ErrUnknownRecipeOperation, s.Op)
	}

	return nil
}

func validateFitMode(mode string, width, height int) error {
	switch mode {
	case "contain", "scaledown":
		if width == 0 && height == 0 {
			return invalidParam("fit", "at least one of width or height is required", "")
		}
		if width < 0 || height < 0 {
			return invalidParam("fit", "width and height must be positive", "")
		}
	case "cover", "crop", "pad", "squeeze":
		if width <= 0 || height <= 0 {
			return invalidParam("fit", fmt.Sprintf("%s mode requires both width and height", mode), "")
		}
	default:
		return invalidParam("fit", "mode must be one of contain, cover, crop, pad, squeeze, scaledown", mode)
	}

	if width > maxRecipeDimension || height > maxRecipeDimension {
		return invalidParam("fit", fmt.Sprintf("width and height must be <= %d", maxRecipeDimension), "")
	}
	return nil
}

func invalidParam(name, reason, value string) error {
	if value != "" {
		return fmt.Errorf("%w: %w: %s: %s (got %q)",
			ErrInvalidRecipe, ErrInvalidRecipeParameter, name, reason, value)
	}
	return fmt.Errorf("%w: %w: %s: %s",
		ErrInvalidRecipe, ErrInvalidRecipeParameter, name, reason)
}

func strictFloat(value string, min, max float64, name string) (float64, error) {
	v := strings.TrimSpace(value)
	if v == "" {
		return 0, fmt.Errorf("%w: %w: %s: value is required",
			ErrInvalidRecipe, ErrInvalidRecipeParameter, name)
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, fmt.Errorf("%w: %w: %s: value must be a number (got %q)",
			ErrInvalidRecipe, ErrInvalidRecipeParameter, name, value)
	}
	if f < min || f > max {
		return 0, fmt.Errorf("%w: %w: %s: value must be between %g and %g (got %g)",
			ErrInvalidRecipe, ErrInvalidRecipeParameter, name, min, max, f)
	}
	return f, nil
}

func strictInt(value string, min, max int, name string) (int, error) {
	v := strings.TrimSpace(value)
	if v == "" {
		return 0, fmt.Errorf("%w: %w: %s: value is required",
			ErrInvalidRecipe, ErrInvalidRecipeParameter, name)
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%w: %w: %s: value must be an integer (got %q)",
			ErrInvalidRecipe, ErrInvalidRecipeParameter, name, value)
	}
	if n < min || n > max {
		return 0, fmt.Errorf("%w: %w: %s: value must be between %d and %d (got %d)",
			ErrInvalidRecipe, ErrInvalidRecipeParameter, name, min, max, n)
	}
	return n, nil
}

func normalizeOp(op string) string {
	return strings.ToLower(strings.TrimSpace(op))
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

var recipeStepFields = map[string]struct{}{
	"op": {}, "operation": {}, "value": {}, "width": {}, "height": {},
	"x": {}, "y": {}, "mode": {}, "quality": {}, "format": {},
}

// rejectUnknownFields reports keys that are not part of the step schema.
func rejectUnknownFields(data []byte, allowed map[string]struct{}) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	keys := make([]string, 0, len(raw))
	for key := range raw {
		if _, ok := allowed[key]; !ok {
			keys = append(keys, key)
		}
	}
	if len(keys) == 0 {
		return nil
	}
	sort.Strings(keys)
	return fmt.Errorf("unknown step field(s): %s", strings.Join(keys, ", "))
}

// ApplyRecipe executes a recipe against the image at path and returns the
// encoded result. The recipe is validated first, all transformation work is
// done in memory, and encoding happens only after every step succeeded.
// Consequently an error never leaves a partial result behind.
func (k *KritiImages) ApplyRecipe(ctx context.Context, path string, r *Recipe) (*RecipeResult, error) {
	if err := ValidateRecipe(r); err != nil {
		return nil, err
	}

	source := k.getImageSource(path)
	img, format, err := source.GetImage(ctx, path)
	if err != nil {
		return nil, ErrSourceImageNotFound
	}

	imageResult, err := ApplyRecipeToImage(img, format, r)
	if err != nil {
		return nil, err
	}

	buf, err := k.formatTo(imageResult.Image, imageResult.Format, imageResult.Quality)
	if err != nil {
		return nil, err
	}

	return &RecipeResult{
		Buffer:  buf,
		Steps:   imageResult.Steps,
		Width:   imageResult.Width,
		Height:  imageResult.Height,
		Format:  imageResult.Format,
		Quality: imageResult.Quality,
		Bytes:   buf.Len(),
	}, nil
}

// ApplyRecipeToImage executes a recipe against an already decoded image. It is
// useful for tests and for callers that already hold an image. The source is
// never modified; every pixel operation writes to a fresh image.
func ApplyRecipeToImage(src image.Image, sourceFormat string, r *Recipe) (*RecipeImageResult, error) {
	if err := ValidateRecipe(r); err != nil {
		return nil, err
	}
	if src == nil {
		return nil, fmt.Errorf("%w: source image is nil", ErrInvalidImageFormat)
	}
	if src.Bounds().Dx() <= 0 || src.Bounds().Dy() <= 0 {
		return nil, fmt.Errorf("%w: source image is empty", ErrEmptyRecipeResult)
	}

	initialFormat := normalizeFormat(sourceFormat)
	if !isSupportedFormat(initialFormat) {
		return nil, fmt.Errorf("%w: %w: source format %q",
			ErrInvalidImageFormat, ErrInvalidImageFormat, sourceFormat)
	}

	state := recipeState{
		image:   src,
		format:  initialFormat,
		quality: 100,
	}
	reports := make([]RecipeStepReport, 0, len(r.Steps))

	for i := range r.Steps {
		if err := state.execute(r.Steps[i]); err != nil {
			return nil, &RecipeStepError{Index: i, Op: r.Steps[i].Op, Err: err}
		}

		bounds := state.image.Bounds()
		reports = append(reports, RecipeStepReport{
			Index:  i,
			Op:     normalizeOp(r.Steps[i].Op),
			Width:  bounds.Dx(),
			Height: bounds.Dy(),
			Format: state.format,
		})
	}

	final := state.image

	return &RecipeImageResult{
		Image:   final,
		Steps:   reports,
		Width:   final.Bounds().Dx(),
		Height:  final.Bounds().Dy(),
		Format:  state.format,
		Quality: state.quality,
	}, nil
}

// DryRunRecipe validates a recipe against a real image without encoding or
// writing anything. It reports the geometry/format after every step, which
// lets callers catch image-dependent failures (for example a step that would
// produce an empty image) before spending time on the final encode.
func DryRunRecipe(src image.Image, sourceFormat string, r *Recipe) (*RecipeImageResult, error) {
	return ApplyRecipeToImage(src, sourceFormat, r)
}

// PreviewRecipe is DryRunRecipe for an image addressed by path: it resolves the
// configured image source, reads the image, and reports what each step would
// produce without encoding or writing any output.
func (k *KritiImages) PreviewRecipe(ctx context.Context, path string, r *Recipe) (*RecipeImageResult, error) {
	if err := ValidateRecipe(r); err != nil {
		return nil, err
	}

	img, format, err := k.getImageSource(path).GetImage(ctx, path)
	if err != nil {
		return nil, ErrSourceImageNotFound
	}
	return ApplyRecipeToImage(img, format, r)
}

// WriteRecipe applies a recipe and writes its output to w. Nothing is written
// unless every step succeeded and the image was fully encoded.
func (k *KritiImages) WriteRecipe(ctx context.Context, path string, r *Recipe, w io.Writer) (*RecipeResult, error) {
	result, err := k.ApplyRecipe(ctx, path, r)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(result.Buffer.Bytes()); err != nil {
		return nil, fmt.Errorf("%w: failed to write recipe output: %w", ErrRecipeWriteFailed, err)
	}
	return result, nil
}

// ApplyRecipeBatch applies one validated recipe to several images. The recipe
// and batch size are checked before any image is read. Individual image
// failures are reported per item; they do not discard successful items, and a
// failed item never carries partial image data.
func (k *KritiImages) ApplyRecipeBatch(ctx context.Context, paths []string, r *Recipe) ([]RecipeBatchItem, error) {
	if err := ValidateRecipe(r); err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("%w: %w", ErrInvalidRecipe, ErrEmptyBatch)
	}
	if len(paths) > MaxRecipeBatchSize {
		return nil, fmt.Errorf("%w: %w: got %d images, max %d",
			ErrInvalidRecipe, ErrBatchTooLarge, len(paths), MaxRecipeBatchSize)
	}

	items := make([]RecipeBatchItem, len(paths))
	for i, path := range paths {
		items[i] = RecipeBatchItem{Index: i, Path: path}
		result, err := k.ApplyRecipe(ctx, path, r)
		if err != nil {
			items[i].Error = err.Error()
			items[i].ErrorCode = RecipeErrorCode(err)
			continue
		}
		items[i].Result = result
	}
	return items, nil
}

// RecipeErrorCode maps an error to a stable, machine-readable code suitable
// for API responses and batch reports.
func RecipeErrorCode(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrSourceImageNotFound):
		return "source_not_found"
	case errors.Is(err, ErrEmptyRecipeResult):
		return "empty_image"
	case errors.Is(err, ErrCropOutOfBounds):
		return "crop_out_of_bounds"
	case errors.Is(err, ErrUnsupportedRecipeVersion):
		return "unsupported_version"
	case errors.Is(err, ErrUnknownRecipeOperation):
		return "unknown_operation"
	case errors.Is(err, ErrRecipeTooLarge):
		return "recipe_too_large"
	case errors.Is(err, ErrBatchTooLarge):
		return "batch_too_large"
	case errors.Is(err, ErrEmptyBatch):
		return "empty_batch"
	case errors.Is(err, ErrInvalidImageFormat), errors.Is(err, ErrFailedToEncodeImage):
		return "unsupported_format"
	case errors.Is(err, ErrRecipeWriteFailed):
		return "write_failed"
	case errors.Is(err, ErrInvalidRecipeParameter):
		return "invalid_parameter"
	case errors.Is(err, ErrInvalidRecipe):
		return "invalid_recipe"
	default:
		return "internal_error"
	}
}

type recipeState struct {
	image         image.Image
	format        string
	quality       int
	background    color.Color
	hasBackground bool
}

func (s *recipeState) execute(step RecipeStep) error {
	op := normalizeOp(step.Op)

	switch op {
	case OpFlip, OpBlur, OpBrightness, OpContrast, OpFit, OpGamma, OpRotate,
		OpSaturation, OpSharpen, OpWidth, OpHeight, OpRadius, OpResize, OpCrop:
		// Image operations are handled below.

	case OpBackground:
		bg, err := utils.ParseBackgroundColor(step.Value)
		if err != nil {
			return fmt.Errorf("%w: %w: %v", ErrInvalidRecipe, ErrInvalidRecipeParameter, err)
		}
		s.background = bg
		s.hasBackground = true
		// Background is not a pixel operation on its own: it is the fill color
		// used by later pad/rotate steps, matching the single-step API where
		// `background` alone does not repaint a transparent image.
		return nil

	case OpFormat:
		value := firstNonEmpty(step.Format, step.Value)
		format, err := utils.ParseFormatValue(value)
		if err != nil {
			return fmt.Errorf("%w: %w: %w", ErrInvalidRecipe, ErrInvalidRecipeParameter, ErrInvalidImageFormat)
		}
		s.format = format
		return nil

	case OpQuality:
		value := step.Value
		if step.Quality > 0 {
			value = strconv.Itoa(step.Quality)
		}
		q, err := strictInt(value, 1, 100, "quality")
		if err != nil {
			return err
		}
		s.quality = q
		return nil

	default:
		return fmt.Errorf("%w: %w: unknown operation %q",
			ErrInvalidRecipe, ErrUnknownRecipeOperation, step.Op)
	}

	filters, err := stepFilters(step, s.image.Bounds(), s.background, s.hasBackground)
	if err != nil {
		return err
	}
	if len(filters) == 0 {
		return nil
	}

	g := gift.New(filters...)
	bounds := g.Bounds(s.image.Bounds())
	if bounds.Dx() <= 0 || bounds.Dy() <= 0 {
		return fmt.Errorf("%w: step would produce a %dx%d image",
			ErrEmptyRecipeResult, bounds.Dx(), bounds.Dy())
	}

	dst := image.NewRGBA(bounds)
	g.Draw(dst, s.image)
	s.image = dst
	return nil
}

func stepFilters(step RecipeStep, srcBounds image.Rectangle, bg color.Color, hasBg bool) ([]gift.Filter, error) {
	op := normalizeOp(step.Op)
	filters := make([]gift.Filter, 0, 2)

	switch op {
	case OpFlip:
		switch strings.ToLower(strings.TrimSpace(step.Value)) {
		case "h":
			filters = append(filters, gift.FlipHorizontal())
		case "v":
			filters = append(filters, gift.FlipVertical())
		case "hv", "vh":
			filters = append(filters, gift.FlipHorizontal(), gift.FlipVertical())
		default:
			return nil, invalidParam("flip", "value must be one of h, v, hv", step.Value)
		}

	case OpBlur:
		v, err := strictFloat(step.Value, 1, 250, "blur")
		if err != nil {
			return nil, err
		}
		filters = append(filters, gift.GaussianBlur(float32(v)))

	case OpBrightness:
		v, err := strictFloat(step.Value, -100, 100, "brightness")
		if err != nil {
			return nil, err
		}
		filters = append(filters, gift.Brightness(float32(v)))

	case OpContrast:
		v, err := strictFloat(step.Value, -100, 100, "contrast")
		if err != nil {
			return nil, err
		}
		filters = append(filters, gift.Contrast(float32(v)))

	case OpGamma:
		v, err := strictFloat(step.Value, 0.1, 2.0, "gamma")
		if err != nil {
			return nil, err
		}
		filters = append(filters, gift.Gamma(float32(v)))

	case OpSaturation:
		v, err := strictFloat(step.Value, -100, 500, "saturation")
		if err != nil {
			return nil, err
		}
		filters = append(filters, gift.Saturation(float32(v)))

	case OpSharpen:
		v, err := strictFloat(step.Value, 0.5, 1.5, "sharpen")
		if err != nil {
			return nil, err
		}
		filters = append(filters, gift.UnsharpMask(1.0, float32(v), 0.0))

	case OpRotate:
		angle, err := utils.ParseRotateAngle(step.Value)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrInvalidRecipe, err)
		}
		fill := color.Color(color.Transparent)
		if hasBg && bg != nil {
			fill = bg
		}
		filters = append(filters, gift.Rotate(angle, fill, gift.LinearInterpolation))

	case OpRadius:
		f, err := transformations.CreateBorderRadiusFilter(step.Value)
		if err != nil {
			return nil, fmt.Errorf("%w: %w: %v", ErrInvalidRecipe, ErrInvalidRecipeParameter, err)
		}
		filters = append(filters, f)

	case OpWidth:
		value := step.Value
		if step.Width > 0 {
			value = strconv.Itoa(step.Width)
		}
		width, err := strictInt(value, 1, maxRecipeDimension, "width")
		if err != nil {
			return nil, err
		}
		filters = append(filters, gift.Resize(width, 0, gift.LanczosResampling))

	case OpHeight:
		value := step.Value
		if step.Height > 0 {
			value = strconv.Itoa(step.Height)
		}
		height, err := strictInt(value, 1, maxRecipeDimension, "height")
		if err != nil {
			return nil, err
		}
		filters = append(filters, gift.Resize(0, height, gift.LanczosResampling))

	case OpResize, OpFit:
		mode := strings.ToLower(strings.TrimSpace(step.Mode))
		if mode == "" {
			mode = strings.ToLower(strings.TrimSpace(step.Value))
		}
		if mode == "" {
			mode = "contain"
		}
		f, err := transformations.CreateFitFilter(mode, step.Width, step.Height, backgroundOrTransparent(bg, hasBg))
		if err != nil {
			return nil, fmt.Errorf("%w: %w: %v", ErrInvalidRecipe, ErrInvalidRecipeParameter, err)
		}
		if f != nil {
			filters = append(filters, f)
		}

	case OpCrop:
		// Crop is a true region crop (unlike fit=crop, which scales the source
		// to fill). This makes sequences of crops meaningful: a second crop is
		// evaluated against the already-cropped image and can therefore fall
		// outside it.
		if step.Width <= 0 || step.Height <= 0 {
			return nil, invalidParam("crop", "both width and height are required", "")
		}
		rect := image.Rect(
			srcBounds.Min.X+step.X,
			srcBounds.Min.Y+step.Y,
			srcBounds.Min.X+step.X+step.Width,
			srcBounds.Min.Y+step.Y+step.Height,
		)
		if rect.Intersect(srcBounds).Empty() {
			// Two crops that do not overlap leave no pixels behind. Report it
			// as an empty result instead of encoding a zero-sized image.
			return nil, fmt.Errorf("%w: crop %v has no pixels inside the current image %v",
				ErrEmptyRecipeResult, rect, srcBounds)
		}
		if !rect.In(srcBounds) {
			// Clamping a partially overlapping crop would silently change the
			// requested size, making the stored recipe produce a different
			// result for this image. Fail instead of guessing.
			return nil, fmt.Errorf("%w: crop %v is not fully inside the current image %v",
				ErrCropOutOfBounds, rect, srcBounds)
		}
		filters = append(filters, gift.Crop(rect))

	default:
		return nil, fmt.Errorf("%w: %w: unknown operation %q",
			ErrInvalidRecipe, ErrUnknownRecipeOperation, step.Op)
	}

	return filters, nil
}

func backgroundOrTransparent(bg color.Color, hasBg bool) color.Color {
	if hasBg && bg != nil {
		return bg
	}
	return color.Transparent
}

func normalizeFormat(format string) string {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "jpg", "jpeg":
		return "jpeg"
	case "png":
		return "png"
	case "webp":
		return "webp"
	default:
		return strings.ToLower(strings.TrimSpace(format))
	}
}

func isSupportedFormat(format string) bool {
	switch normalizeFormat(format) {
	case "jpeg", "png", "webp":
		return true
	default:
		return false
	}
}
