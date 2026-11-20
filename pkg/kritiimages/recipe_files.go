package kritiimages

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// WriteRecipeFiles applies a recipe to each source/destination pair and writes
// the encoded output to destination files.
//
// Writes are staged: every output is first written to a temporary file in the
// destination directory, and only when *all* images have been produced
// successfully are the temporary files renamed into place. Files are closed
// immediately after they are written, so a failure never leaves a partially
// written image behind. On error, any temporary files created by this call are
// removed and no destination file is created or modified.
//
// Destinations must be distinct; duplicate paths are rejected up-front.
func (k *KritiImages) WriteRecipeFiles(ctx context.Context, sources, destinations []string, r *Recipe) ([]RecipeBatchItem, error) {
	if err := ValidateRecipe(r); err != nil {
		return nil, err
	}
	if len(sources) == 0 {
		return nil, fmt.Errorf("%w: %w", ErrInvalidRecipe, ErrEmptyBatch)
	}
	if len(sources) != len(destinations) {
		return nil, fmt.Errorf("%w: %w: got %d sources and %d destinations",
			ErrInvalidRecipe, ErrInvalidRecipeParameter, len(sources), len(destinations))
	}
	if len(sources) > MaxRecipeBatchSize {
		return nil, fmt.Errorf("%w: %w: got %d images, max %d",
			ErrInvalidRecipe, ErrBatchTooLarge, len(sources), MaxRecipeBatchSize)
	}

	seen := make(map[string]struct{}, len(destinations))
	for _, dst := range destinations {
		if dst == "" {
			return nil, fmt.Errorf("%w: %w: destination path is empty",
				ErrInvalidRecipe, ErrInvalidRecipeParameter)
		}
		clean := filepath.Clean(dst)
		if _, ok := seen[clean]; ok {
			return nil, fmt.Errorf("%w: %w: duplicate destination %q",
				ErrInvalidRecipe, ErrInvalidRecipeParameter, dst)
		}
		seen[clean] = struct{}{}
	}

	// Produce every image in memory first. This is the expensive part, but it
	// is also the part that can fail without touching the filesystem.
	items := make([]RecipeBatchItem, len(sources))
	for i, src := range sources {
		items[i] = RecipeBatchItem{Index: i, Path: src}
		result, err := k.ApplyRecipe(ctx, src, r)
		if err != nil {
			return nil, fmt.Errorf("image %d (%s): %w", i, src, err)
		}
		items[i].Result = result
	}

	temps := make([]string, 0, len(destinations))
	cleanupTemps := func() {
		for _, tmp := range temps {
			_ = os.Remove(tmp)
		}
	}

	for i, dst := range destinations {
		temp, err := writeTempFile(dst, items[i].Result.Buffer.Bytes())
		if err != nil {
			cleanupTemps()
			return nil, fmt.Errorf("%w: %d: %w", ErrRecipeWriteFailed, i, err)
		}
		temps = append(temps, temp)
	}

	// Move any file that is about to be overwritten aside, so a failure while
	// committing can put the previous contents back instead of deleting the
	// caller's data.
	type backup struct {
		dst string
		old string
	}
	backups := make([]backup, 0, len(destinations))
	restoreBackups := func() {
		for _, b := range backups {
			_ = os.Rename(b.old, b.dst)
		}
	}

	for _, dst := range destinations {
		if _, err := os.Stat(dst); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			restoreBackups()
			cleanupTemps()
			return nil, fmt.Errorf("%w: failed to inspect destination %q: %v",
				ErrRecipeWriteFailed, dst, err)
		}

		old, err := reserveTempName(filepath.Dir(dst))
		if err != nil {
			restoreBackups()
			cleanupTemps()
			return nil, fmt.Errorf("%w: failed to stage backup for %q: %v",
				ErrRecipeWriteFailed, dst, err)
		}
		if err := os.Rename(dst, old); err != nil {
			restoreBackups()
			cleanupTemps()
			return nil, fmt.Errorf("%w: failed to back up %q: %v",
				ErrRecipeWriteFailed, dst, err)
		}
		backups = append(backups, backup{dst: dst, old: old})
	}

	committed := make([]string, 0, len(destinations))
	for i, dst := range destinations {
		if err := os.Rename(temps[i], dst); err != nil {
			for _, done := range committed {
				_ = os.Remove(done)
			}
			restoreBackups()
			cleanupTemps()
			return nil, fmt.Errorf("%w: failed to commit %q: %v",
				ErrRecipeWriteFailed, dst, err)
		}
		committed = append(committed, dst)
	}

	// Success: the previous contents are no longer needed.
	for _, b := range backups {
		_ = os.Remove(b.old)
	}

	return items, nil
}

// reserveTempName creates and immediately removes a uniquely named file in
// dir, returning a name that is safe to rename over.
func reserveTempName(dir string) (string, error) {
	f, err := os.CreateTemp(dir, ".kriti-recipe-backup-*")
	if err != nil {
		return "", err
	}
	name := f.Name()
	if err := f.Close(); err != nil {
		os.Remove(name)
		return "", err
	}
	if err := os.Remove(name); err != nil {
		return "", err
	}
	return name, nil
}

func writeTempFile(dst string, data []byte) (string, error) {
	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("failed to create directory %q: %w", dir, err)
	}

	f, err := os.CreateTemp(dir, ".kriti-recipe-*")
	if err != nil {
		return "", fmt.Errorf("failed to create temporary file in %q: %w", dir, err)
	}
	name := f.Name()

	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(name)
		return "", fmt.Errorf("failed to write temporary file: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(name)
		return "", fmt.Errorf("failed to sync temporary file: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(name)
		return "", fmt.Errorf("failed to close temporary file: %w", err)
	}

	return name, nil
}
