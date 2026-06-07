package ondemand

import (
	"archive/tar"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"

	"github.com/klauspost/compress/zstd"
)

func CompressFolder(name string) (string, error) {
	return compressFolderWithZst(name)
}

func compressFolderWithZst(folder string) (string, error) {
	// Validation
	stat, statErr := os.Stat(folder)
	if statErr != nil {
		return "", statErr
	}

	if !stat.IsDir() {
		return "", fmt.Errorf("input is not a valid directory: %s", folder)
	}

	outputName := fmt.Sprintf("%s.zst", folder)
	out, err := os.Create(outputName)
	if err != nil {
		return "", err
	}
	defer out.Close()

	const zstdLevel = 1
	const zstdEncoderConcurrency = 1

	enc, err := zstd.NewWriter(out,
		zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(zstdLevel)),
		zstd.WithEncoderConcurrency(zstdEncoderConcurrency),
	)

	if err != nil {
		return "", err
	}

	defer enc.Close()

	tarWriter := tar.NewWriter(enc)
	defer tarWriter.Close()

	// Resolve folder base name
	absFolderPath, err := filepath.Abs(folder)
	if err != nil {
		return "", err
	}
	folderBaseName := filepath.Base(absFolderPath)

	walkErr := filepath.Walk(folder, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}

		absPath, err := filepath.Abs(p)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(absFolderPath, absPath)
		if err != nil {
			return err
		}
		entryName := path.Join(folderBaseName, filepath.ToSlash(rel))

		fiHdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		fiHdr.Name = entryName

		if err := tarWriter.WriteHeader(fiHdr); err != nil {
			return fmt.Errorf("write header for %s: %w", entryName, err)
		}

		if info.Mode().IsRegular() {
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			_, err = io.Copy(tarWriter, f)
			cerr := f.Close()
			if err != nil {
				return fmt.Errorf("copy %s: %w", entryName, err)
			}
			if cerr != nil {
				return cerr
			}
		}
		return nil
	})

	if walkErr != nil {
		return "", walkErr
	}

	return outputName, nil
}
