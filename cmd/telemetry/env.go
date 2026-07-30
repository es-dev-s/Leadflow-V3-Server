package main

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

func loadEnvFile(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		if os.Getenv(key) == "" {
			_ = os.Setenv(key, val)
		}
	}
}

func loadEnvFromCRM() {
	// Prefer cwd .env, then backend/.env when launched from repo root.
	loadEnvFile(".env")
	loadEnvFile(filepath.Join("backend", ".env"))
	loadEnvFile(filepath.Join("..", ".env"))
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}
