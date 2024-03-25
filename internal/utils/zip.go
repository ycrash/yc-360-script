package utils

import (
	"archive/zip"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
)

func ZipFolder(folder string) (name string, err error) {
	name = fmt.Sprintf("%s.zip", folder)
	file, err := os.Create(name)
	if err != nil {
		return
	}
	defer file.Close()

	w := zip.NewWriter(file)
	defer w.Close()

	folderBaseName := filepath.Base(folder)

	walker := func(filePath string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		file, err := os.Open(filePath)
		if err != nil {
			return err
		}
		defer file.Close()

		// filePath can contain absolute path, so let's get the base path.
		fileBaseName := filepath.Base(filePath)

		// zipFileName is in the form of folder/filename.ext
		// If otherwise filePath is used here, it can contains an absolute path, which results in
		// unexpected zip contents.
		zipFileName := path.Join(folderBaseName, fileBaseName)

		f, err := w.Create(zipFileName)
		if err != nil {
			return err
		}

		_, err = io.Copy(f, file)
		if err != nil {
			return err
		}

		return nil
	}
	err = filepath.Walk(folder, walker)
	return
}
