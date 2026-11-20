package routes

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/log"
	"github.com/kritihq/kriti-images/pkg/kritiimages"
)

// BindRouteRecipes registers the reusable transformation recipe API.
//
// The endpoints are additive: the existing /cgi/images route and its default
// output are untouched.
//
//	POST /api/v0/recipes/validate   validate a recipe, no image required
//	POST /api/v0/recipes/preview/:image  dry run, report each step's size
//	POST /api/v0/recipes/apply/:image    apply and return the image
//	POST /api/v0/recipes/batch      apply to several images, report each
//
// The request body is either a recipe object or {"recipe": <recipe>}.
func BindRouteRecipes(server *fiber.App, k *kritiimages.KritiImages) {
	server.Post("/api/v0/recipes/validate", func(c *fiber.Ctx) error {
		recipe, err := decodeRecipeRequest(c.Body())
		if err != nil {
			return recipeError(c, err)
		}
		if err := recipe.Validate(); err != nil {
			return recipeError(c, err)
		}
		return c.Status(http.StatusOK).JSON(fiber.Map{
			"valid":   true,
			"version": recipe.Version,
			"steps":   len(recipe.Steps),
		})
	})

	server.Post(`/api/v0/recipes/preview/:image`, func(c *fiber.Ctx) error {
		imagePath, err := unescapeImagePath(c.Params("image", ""))
		if err != nil {
			return c.Status(http.StatusBadRequest).JSON(fiber.Map{
				"error": "invalid image path",
				"code":  "invalid_request",
			})
		}
		imagePath = maybeUnescapeURL(imagePath)

		recipe, err := decodeRecipeRequest(c.Body())
		if err != nil {
			return recipeError(c, err)
		}

		preview, err := k.PreviewRecipe(c.Context(), imagePath, recipe)
		if err != nil {
			return recipeError(c, err)
		}

		return c.Status(http.StatusOK).JSON(fiber.Map{
			"width":   preview.Width,
			"height":  preview.Height,
			"format":  preview.Format,
			"quality": preview.Quality,
			"steps":   preview.Steps,
		})
	})

	server.Post(`/api/v0/recipes/apply/:image`, func(c *fiber.Ctx) error {
		imagePath, err := unescapeImagePath(c.Params("image", ""))
		if err != nil {
			return c.Status(http.StatusBadRequest).JSON(fiber.Map{
				"error": "invalid image path",
				"code":  "invalid_request",
			})
		}
		imagePath = maybeUnescapeURL(imagePath)

		recipe, err := decodeRecipeRequest(c.Body())
		if err != nil {
			return recipeError(c, err)
		}

		result, err := k.ApplyRecipe(c.Context(), imagePath, recipe)
		if err != nil {
			return recipeError(c, err)
		}

		contentType, err := contentTypeFor(result.Format)
		if err != nil {
			return recipeError(c, err)
		}

		stepReport, err := json.Marshal(result.Steps)
		if err != nil {
			return recipeError(c, err)
		}

		c.Set("Content-Type", contentType)
		c.Set("X-Recipe-Format", result.Format)
		c.Set("X-Recipe-Width", fmt.Sprintf("%d", result.Width))
		c.Set("X-Recipe-Height", fmt.Sprintf("%d", result.Height))
		c.Set("X-Recipe-Quality", fmt.Sprintf("%d", result.Quality))
		c.Set("X-Recipe-Steps", string(stepReport))

		// Same CDN behavior as the single-step route.
		c.Set("Cache-Control", "public, max-age=31536000, immutable")
		c.Set("Expires", time.Now().Add(time.Hour*24*365).UTC().Format(http.TimeFormat))
		c.Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
		c.Set("Vary", "Accept")
		c.Set("X-Content-Type-Options", "nosniff")
		c.Set("Content-Security-Policy", "default-src 'none'")
		c.Set("X-Robots-Tag", "noindex, nofollow")
		c.Set("Access-Control-Allow-Origin", "*")

		return c.Status(http.StatusOK).Send(result.Buffer.Bytes())
	})

	server.Post("/api/v0/recipes/batch", func(c *fiber.Ctx) error {
		var req struct {
			Recipe  *kritiimages.Recipe `json:"recipe"`
			Images  []string            `json:"images"`
			Outputs []string            `json:"outputs"`
			Report  bool                `json:"report"`
		}
		if err := json.Unmarshal(c.Body(), &req); err != nil {
			return c.Status(http.StatusBadRequest).JSON(fiber.Map{
				"error": fmt.Sprintf("invalid request body: %s", err.Error()),
				"code":  "invalid_request",
			})
		}
		if req.Recipe == nil {
			return c.Status(http.StatusBadRequest).JSON(fiber.Map{
				"error": "recipe is required",
				"code":  "invalid_recipe",
			})
		}

		if len(req.Outputs) > 0 {
			// Writing to disk is an explicit opt-in: callers must provide one
			// output per input. The write is all-or-nothing.
			items, err := k.WriteRecipeFiles(c.Context(), req.Images, req.Outputs, req.Recipe)
			if err != nil {
				return recipeError(c, err)
			}
			return c.Status(http.StatusCreated).JSON(fiber.Map{
				"count": len(items),
				"items": batchReports(items),
			})
		}

		items, err := k.ApplyRecipeBatch(c.Context(), req.Images, req.Recipe)
		if err != nil {
			return recipeError(c, err)
		}
		return c.Status(http.StatusOK).JSON(fiber.Map{
			"count": len(items),
			"items": batchReports(items),
		})
	})
}

func decodeRecipeRequest(body []byte) (*kritiimages.Recipe, error) {
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, fmt.Errorf("%w: request body is empty", kritiimages.ErrInvalidRecipe)
	}

	// Accept either a bare recipe or a wrapper object with a "recipe" field.
	var wrapper struct {
		Recipe *kritiimages.Recipe `json:"recipe"`
	}
	if err := json.Unmarshal(body, &wrapper); err == nil && wrapper.Recipe != nil {
		return wrapper.Recipe, nil
	}

	var recipe kritiimages.Recipe
	if err := json.Unmarshal(body, &recipe); err != nil {
		return nil, fmt.Errorf("%w: %w", kritiimages.ErrInvalidRecipe, err)
	}
	return &recipe, nil
}

func batchReports(items []kritiimages.RecipeBatchItem) []fiber.Map {
	reports := make([]fiber.Map, 0, len(items))
	for _, item := range items {
		report := fiber.Map{
			"index": item.Index,
			"path":  item.Path,
		}
		if item.Result != nil {
			report["width"] = item.Result.Width
			report["height"] = item.Result.Height
			report["format"] = item.Result.Format
			report["quality"] = item.Result.Quality
			report["bytes"] = item.Result.Bytes
			report["steps"] = item.Result.Steps
		}
		if item.Error != "" {
			report["error"] = item.Error
			report["error_code"] = item.ErrorCode
		}
		reports = append(reports, report)
	}
	return reports
}

func recipeError(c *fiber.Ctx, err error) error {
	status := http.StatusBadRequest
	switch {
	case errors.Is(err, kritiimages.ErrBatchTooLarge):
		status = http.StatusRequestEntityTooLarge
	case errors.Is(err, kritiimages.ErrSourceImageNotFound):
		status = http.StatusNotFound
	case errors.Is(err, kritiimages.ErrRecipeWriteFailed):
		status = http.StatusInternalServerError
	}

	code := kritiimages.RecipeErrorCode(err)
	log.Warnw("recipe request failed", "code", code, "error", err.Error())
	return c.Status(status).JSON(fiber.Map{
		"error": err.Error(),
		"code":  code,
	})
}

func unescapeImagePath(raw string) (string, error) {
	if raw == "" {
		return "", fmt.Errorf("image parameter is required")
	}
	unescaped, err := url.PathUnescape(raw)
	if err != nil {
		return "", err
	}
	return unescaped, nil
}

// maybeUnescapeURL lets callers pass an escaped URL as the image path, the
// same way the single-step route documents it.
func maybeUnescapeURL(path string) string {
	lower := strings.ToLower(path)
	if strings.Contains(lower, "http%3a") || strings.Contains(lower, "https%3a") {
		if unescaped, err := url.QueryUnescape(path); err == nil {
			return unescaped
		}
	}
	return path
}

func contentTypeFor(format string) (string, error) {
	switch strings.ToLower(format) {
	case "jpg", "jpeg":
		return "image/jpeg", nil
	case "png":
		return "image/png", nil
	case "webp":
		return "image/webp", nil
	default:
		return "", kritiimages.ErrInvalidImageFormat
	}
}
