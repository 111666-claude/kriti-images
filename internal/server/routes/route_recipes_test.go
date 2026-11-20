package routes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/kritihq/kriti-images/pkg/kritiimages"
)

type stubSource struct {
	images  map[string]image.Image
	formats map[string]string
}

func (s *stubSource) GetImage(_ context.Context, name string) (image.Image, string, error) {
	img, ok := s.images[name]
	if !ok {
		return nil, "", errors.New("not found")
	}
	return img, s.formats[name], nil
}

func (s *stubSource) UploadImage(context.Context, string, image.Image) error { return nil }

func testImage(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, color.RGBA{uint8(x * 20), uint8(y * 20), 90, 255})
		}
	}
	return img
}

func newRecipeTestApp(t *testing.T) *fiber.App {
	t.Helper()
	src := &stubSource{
		images: map[string]image.Image{
			"cover.png": testImage(8, 6),
		},
		formats: map[string]string{"cover.png": "png"},
	}
	service := kritiimages.New(map[string]kritiimages.ImageSource{"local": src}, src)
	app := fiber.New()
	BindRouteRecipes(app, service)
	return app
}

func postJSON(t *testing.T, app *fiber.App, path string, body string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	return resp
}

const validRecipeJSON = `{
	"version": 1,
	"steps": [
		{"op": "crop", "width": 4, "height": 6},
		{"op": "flip", "value": "h"},
		{"op": "resize", "width": 2, "mode": "contain"},
		{"op": "format", "format": "png"}
	]
}`

func TestRecipeValidateEndpoint(t *testing.T) {
	app := newRecipeTestApp(t)

	resp := postJSON(t, app, "/api/v0/recipes/validate", validRecipeJSON)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["valid"] != true || body["steps"].(float64) != 4 || body["version"].(float64) != 1 {
		t.Fatalf("unexpected body: %v", body)
	}

	resp = postJSON(t, app, "/api/v0/recipes/validate",
		`{"version":1,"steps":[{"op":"unknown_op","value":"1"}]}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	var errBody map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&errBody); err != nil {
		t.Fatal(err)
	}
	if errBody["code"] != "unknown_operation" {
		t.Fatalf("code = %q, want unknown_operation", errBody["code"])
	}
}

func TestRecipePreviewEndpointReportsSizes(t *testing.T) {
	app := newRecipeTestApp(t)

	resp := postJSON(t, app, "/api/v0/recipes/preview/cover.png", validRecipeJSON)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body struct {
		Width  int                            `json:"width"`
		Height int                            `json:"height"`
		Format string                         `json:"format"`
		Steps  []kritiimages.RecipeStepReport `json:"steps"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Width != 2 || body.Height != 3 || body.Format != "png" {
		t.Fatalf("unexpected result: %+v", body)
	}
	if len(body.Steps) != 4 {
		t.Fatalf("reported %d steps, want 4", len(body.Steps))
	}
	if body.Steps[0].Width != 4 || body.Steps[0].Height != 6 {
		t.Fatalf("crop step reported %dx%d, want 4x6", body.Steps[0].Width, body.Steps[0].Height)
	}
	if body.Steps[2].Width != 2 || body.Steps[2].Height != 3 {
		t.Fatalf("resize step reported %dx%d, want 2x3", body.Steps[2].Width, body.Steps[2].Height)
	}
}

func TestRecipePreviewEmptyImageIsReported(t *testing.T) {
	app := newRecipeTestApp(t)
	body := `{"version":1,"steps":[
		{"op":"crop","width":4,"height":4},
		{"op":"crop","x":100,"y":100,"width":4,"height":4}
	]}`
	resp := postJSON(t, app, "/api/v0/recipes/preview/cover.png", body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	var errBody map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&errBody); err != nil {
		t.Fatal(err)
	}
	if errBody["code"] != "empty_image" {
		t.Fatalf("code = %q, want empty_image", errBody["code"])
	}
}

func TestRecipeApplyEndpointReturnsImage(t *testing.T) {
	app := newRecipeTestApp(t)

	resp := postJSON(t, app, "/api/v0/recipes/apply/cover.png", validRecipeJSON)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "image/png" {
		t.Fatalf("content type = %q, want image/png", ct)
	}
	if resp.Header.Get("X-Recipe-Width") != "2" || resp.Header.Get("X-Recipe-Height") != "3" {
		t.Fatalf("unexpected size headers: %v", resp.Header)
	}
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		t.Fatal(err)
	}
	img, format, err := image.Decode(&buf)
	if err != nil {
		t.Fatalf("response is not an image: %v", err)
	}
	if format != "png" || img.Bounds().Dx() != 2 || img.Bounds().Dy() != 3 {
		t.Fatalf("image = %s %dx%d, want png 2x3", format, img.Bounds().Dx(), img.Bounds().Dy())
	}

	for _, tc := range []struct {
		name string
		body string
		want int
		code string
	}{
		{"unknown op", `{"version":1,"steps":[{"op":"explode"}]}`, http.StatusBadRequest, "unknown_operation"},
		{"out of range", `{"version":1,"steps":[{"op":"blur","value":"500"}]}`, http.StatusBadRequest, "invalid_parameter"},
		{"bad format", `{"version":1,"steps":[{"op":"format","format":"gif"}]}`, http.StatusBadRequest, "unsupported_format"},
		{"bad version", `{"version":99,"steps":[{"op":"blur","value":"2"}]}`, http.StatusBadRequest, "unsupported_version"},
		{"empty body", ``, http.StatusBadRequest, "invalid_recipe"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := postJSON(t, app, "/api/v0/recipes/apply/cover.png", tc.body)
			if resp.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.want)
			}
			var errBody map[string]string
			if err := json.NewDecoder(resp.Body).Decode(&errBody); err != nil {
				t.Fatal(err)
			}
			if errBody["code"] != tc.code {
				t.Fatalf("code = %q, want %q", errBody["code"], tc.code)
			}
		})
	}

	resp = postJSON(t, app, "/api/v0/recipes/apply/missing.png", validRecipeJSON)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("missing image status = %d, want 404", resp.StatusCode)
	}
}

func TestRecipeBatchEndpointLimits(t *testing.T) {
	app := newRecipeTestApp(t)

	images := make([]string, kritiimages.MaxRecipeBatchSize+1)
	for i := range images {
		images[i] = "cover.png"
	}
	payload, err := json.Marshal(map[string]any{
		"recipe": json.RawMessage(validRecipeJSON),
		"images": images,
	})
	if err != nil {
		t.Fatal(err)
	}

	resp := postJSON(t, app, "/api/v0/recipes/batch", string(payload))
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.StatusCode)
	}
	var errBody map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&errBody); err != nil {
		t.Fatal(err)
	}
	if errBody["code"] != "batch_too_large" {
		t.Fatalf("code = %q, want batch_too_large", errBody["code"])
	}

	// A small batch reports per-image outcomes.
	payload, _ = json.Marshal(map[string]any{
		"recipe": json.RawMessage(validRecipeJSON),
		"images": []string{"cover.png", "missing.png"},
	})
	resp = postJSON(t, app, "/api/v0/recipes/batch", string(payload))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body struct {
		Items []struct {
			Width     int    `json:"width"`
			Format    string `json:"format"`
			ErrorCode string `json:"error_code"`
		} `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.Items) != 2 {
		t.Fatalf("got %d items, want 2", len(body.Items))
	}
	if body.Items[0].Width != 2 || body.Items[0].Format != "png" {
		t.Fatalf("unexpected first item: %+v", body.Items[0])
	}
	if body.Items[1].ErrorCode != "source_not_found" {
		t.Fatalf("unexpected second item: %+v", body.Items[1])
	}
}
