# Kriti Images

A high-performance image transformation service built in Go, providing a URL-based API for real-time image processing. An open-source alternative to Cloudflare Images and ImageKit.

**[Website](https://kritiimages.com) | [Demo](https://kritiimages.com/docs/transformations)**

### Celebrating 150+ stars. Thank You!

![Star History Chart](https://api.star-history.com/svg?repos=kritihq/kriti-images&type=date&legend=bottom-right)

## 🚀 Features

- **URL-based transformations** - Transform images through simple URL parameters
- **Multiple formats** - Support for JPEG, PNG, and WebP
- **Rich transformations** - Resize, crop, rotate, blur, adjust brightness/contrast, and more
- **Smart resizing modes** - Contains, cover, crop, pad, squeeze, and scale-down options
- **Color adjustments** - Brightness, contrast, saturation, gamma correction
- **Background colors** - Support for hex, RGB, and named colors
- **High performance** - Built with Go and optimized for speed
- **CDN-friendly** - Proper caching headers for optimal CDN integration
- **AWS S3 support** - Store images in AWS S3 and serve them through Kriti Images
- **Use URL for image source** - No need to upload images to storage, provide URL instead

## 📖 Quick Example

Transform an image by modifying the URL:

```bash
# Original image
GET /cgi/images/tr:quality=100/image1.jpg

# Resize to 300px width, auto height
GET /cgi/images/tr:width=300/image1.jpg

# Resize with specific fit mode and background
GET /cgi/images/tr:width=400,height=300,fit=pad,background=blue/image1.jpg

# Multiple transformations
GET /cgi/images/tr:width=500,brightness=20,contrast=30,format=webp/image1.jpg

# Blur with rotation
GET /cgi/images/tr:blur=10,rotate=45,background=white/image1.jpg

# URL based image source, always escape image path
GET /cgi/images/tr:flip=hv/https%3A%2F%2Fimages.unsplash.com%2Fphoto-1764782979306-1e489462d895
```

## 🛠 Supported Transformations

### Resize & Cropping
- `width` - Set image width (1-10000px)
- `height` - Set image height (1-10000px)
- `fit` - Resize behavior: `contain`, `cover`, `crop`, `pad`, `squeeze`, `scaledown`

### Image Adjustments
- `brightness` - Adjust brightness (-100 to 100)
- `contrast` - Adjust contrast (-100 to 100)
- `saturation` - Adjust color saturation (-100 to 500)
- `gamma` - Gamma correction (0.1 to 2.0)
- `blur` - Gaussian blur (1 to 250)
- `sharpen` - Unsharp mask sharpening (0.5 to 1.5)

### Rotation & Flipping
- `rotate` - Rotate image (0-360° or shortcuts: `90`, `cw`, `180`, `270`, `ccw`)
- `flip` - Flip image (`h` for horizontal, `v` for vertical, `hv` for both)

### Visual Effects
- `radius` - Border radius for rounded corners (pixels: `10`, `20px` or percentage: `15%`, `25%`)

### Format & Quality
- `format` - Output format (`jpeg`, `png`, `webp`)
- `quality` - JPEG/WebP quality (1-100, higher = better quality)
- `background` - Background color (hex: `#ff0000`, named: `red`, rgb: `rgb(255,0,0)`)

## 🔧 Upload Images

> **Note**: This functionality is still experimental and _could be removed or moved (api route)_ in future updates. It is disabled by default and must be enabled using configs.

### Enabling Upload APIs

**YAML Configuration:**
```yaml
experimental:
  enable_upload_api: true
```

**TOML Configuration:**
```toml
[experimental]
enable_upload_api = true
```

### New Image

**Endpoint:** `POST /api/v0/images`

**Content-Type:** `multipart/form-data`

**Parameters:**
- `image` (required): The image file to upload
- `filename` (optional): Custom filename for the uploaded image. If not provided, uses the original filename.

**Supported Formats:**
- JPEG (`.jpg`, `.jpeg`)
- PNG (`.png`)
- WebP (`.webp`)

**Example using cURL:**
```bash
curl -X POST http://localhost:8080/api/v0/images \
  -F "image=@/path/to/your/image.jpg" \
  -F "filename=my-custom-name.jpg"
```

### Update Existing Image

**Endpoint:** `PUT /api/v0/images`

**Content-Type:** `multipart/form-data`

**Parameters:**
- `image` (required): The image file to upload
- `filename` (required): The filename of the existing image to update

**Example using cURL:**
```bash
curl -X PUT http://localhost:8080/api/v0/images \
  -F "image=@/path/to/your/new-image.jpg" \
  -F "filename=existing-image.jpg"
```

## 🧪 Reusable Transformation Recipes

A **recipe** is a versioned, ordered list of transformations that can be stored
once and replayed across a whole batch of images. Unlike the URL options (which
live in an unordered map), recipe steps are applied strictly in the order they
are written.

Recipes are plain JSON. The current schema version is `1`; a recipe with any
other version is rejected rather than interpreted with today's semantics.

```json
{
  "version": 1,
  "steps": [
    { "op": "background", "value": "#ffffff" },
    { "op": "resize", "mode": "cover", "width": 600, "height": 600 },
    { "op": "crop", "x": 0, "y": 0, "width": 400, "height": 400 },
    { "op": "brightness", "value": "8" },
    { "op": "radius", "value": "8%" },
    { "op": "format", "format": "webp" },
    { "op": "quality", "quality": 82 }
  ]
}
```

### Operations

| Operation | Fields | Notes |
|---|---|---|
| `width`, `height` | `value` or `width`/`height` | 1–10000, aspect-preserving |
| `resize`, `fit` | `width`, `height`, `mode` | `contain`, `cover`, `crop`, `pad`, `squeeze`, `scaledown` |
| `crop` | `x`, `y`, `width`, `height` | New region crop evaluated against the *current* image; a crop that falls outside it is an error |
| `flip` | `value` | `h`, `v`, `hv` |
| `blur` | `value` | 1–250 |
| `brightness`, `contrast` | `value` | -100–100 |
| `saturation` | `value` | -100–500 |
| `gamma` | `value` | 0.1–2.0 |
| `sharpen` | `value` | 0.5–1.5 |
| `rotate` | `value` | degrees or `cw`, `ccw`, `left`, `right`, `flip` |
| `radius` | `value` | `10`, `20px`, `15%` |
| `background` | `value` | Fill color for later `pad`/`rotate` steps |
| `format` | `format` or `value` | `jpeg`, `png`, `webp` |
| `quality` | `quality` or `value` | 1–100 |

`op` is matched case-insensitively, and a numeric or boolean `value` is accepted
for convenience (e.g. `{"op": "blur", "value": 4}`).

### Validation and dry runs

`ValidateRecipe` checks the schema version, step count, operation names and all
static parameters **without touching an image**. `PreviewRecipe` (or
`DryRunRecipe` for an in-memory image) additionally executes the recipe against
a real image without encoding or writing anything, and reports the width,
height and output format after every step. This catches image-dependent
failures — such as a second crop that falls outside the once-cropped image —
before any output is produced.

### Atomic output

`WriteRecipeFiles` produces every output in memory first, stages each encoded
image in a temporary file next to its destination, and only then renames them
into place. If anything fails — an unreadable source, an invalid recipe, an
empty crop, or a write error — no destination is created or modified. Files
that already existed are restored rather than deleted.

### Batch limits

| Limit | Value |
|---|---|
| Steps per recipe | 64 |
| Images per batch | 100 |

Batches over the limit, and batches with no images, are rejected before any
image is read. Within a valid batch, failures are reported per image so one bad
source does not hide the successful results.

### HTTP API

```bash
# Validate a recipe (no image needed)
curl -X POST http://localhost:8080/api/v0/recipes/validate \
  -H 'Content-Type: application/json' -d @recipe.json

# Dry run: sizes and format after each step, nothing encoded
curl -X POST http://localhost:8080/api/v0/recipes/preview/image1.jpg \
  -H 'Content-Type: application/json' -d @recipe.json

# Apply and return the image
curl -X POST http://localhost:8080/api/v0/recipes/apply/image1.jpg \
  -H 'Content-Type: application/json' -d @recipe.json -o out.webp

# Apply to several images; `outputs` makes the write atomic
curl -X POST http://localhost:8080/api/v0/recipes/batch \
  -H 'Content-Type: application/json' \
  -d '{"recipe": '"$(cat recipe.json)"', "images": ["a.jpg", "b.jpg"]}'
```

Every error response carries a stable `code` such as `unknown_operation`,
`invalid_parameter`, `unsupported_format`, `unsupported_version`, `empty_image`,
`crop_out_of_bounds`, `source_not_found`, `recipe_too_large`, or
`batch_too_large`.

### Converting existing URL options

`RecipeFromOptions` turns an existing `map[TransformationOption]string` plus
`DestinationImage` into a versioned recipe, so a pipeline that already works
through the single-step API can be captured and replayed. The legacy API and
its defaults are unchanged.

## 🏗 Build & Run

### Prerequisites
- Go 1.24.6 or later
- Git
- AWS credentials (Optional)

### Development Setup

1. **Prepare sample images**
   ```bash
   mkdir -p web/static/assets
   # Add your test images to web/static/assets/
   ```

2. **Run the server**
   ```bash
   go run main.go
   ```

3. **Access the service**
   - API: `http://localhost:8080/cgi/images/tr:<transformations>/<image-name>`
   - Demo page: `http://localhost:8080/demo`

   > Always URL escape <image-name> path

### Production Build

```bash
# Build binary
go build -o kriti-images main.go

# Run with production settings
./kriti-images
```

### Docker

Check out [Docker README](docker/README.md) for details.

## 🔧 Configuration

The service uses external configuration files with the [Viper](https://github.com/spf13/viper) library. Configuration can be provided in YAML or TOML format.

### Configuration Files

Create a `config.yaml` or `config.toml` file in the project root.

### Configuration Options

- **server.port** - Server port (default: 8080)
- **server.enable_print_routes** - Enable route debugging (default: false)
- **server.read_timeout** - Request read timeout (default: 30s)
- **server.write_timeout** - Response write timeout (default: 30s)
- **images.source** - Network source, `local` or `awss3` (default: local)
- **images.local.base_path** - Image source directory (default: "")
- **images.aws.s3.bucket** - AWS S3 bucket name (default: "")
- **images.max_image_dimension** - Maximum image dimension, any source image beyond will not be processed (default: 8192 (8K))
- **images.max_file_size_in_bytes** - Maximum image file size, any source image beyond will not be processed (default: 52428800 (50MB))
- **server.limiter.max** - Rate limit per minute (default: 100)
- **server.limiter.expiration** - Rate limit window (default: 1m)
- **experimental.enable_upload_api** - Enable/disable upload APIs (POST/PUT /api/v0/images) (default: false)

> to use `awss3` as `images.source` you must have AWS CLI installed and configured

> configs under `experimental` are temporary and so are the features they relate to. These configs & and the related features could be removed/moved in future releases

## 🌐 API Reference

### Base URL Structure
```
/cgi/images/tr:<transformations>/<image-path>
```

### Transformation Syntax
Multiple transformations are separated by commas:
```
tr:width=300,height=200,format=webp,quality=80
```

## 🚦 Health & Monitoring

- **Health Check**: `GET /health/ready` - Returns 200 when service is ready
- **Liveness Check**: `GET /health/live` - Returns 200 when service is alive
- **Metrics**: `GET /metrics` - Prometheus-compatible metrics


## 📄 License

This project is licensed under the MIT License - see the LICENSE file for details.
