package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

type envFile struct {
	values map[string]string
	order  []string
	issues []string
}

// readEnv parses the same KEY=VALUE subset docker compose accepts.
func readEnv(path string) (*envFile, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	ef := &envFile{values: map[string]string{}}
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			ef.issues = append(ef.issues, fmt.Sprintf("line %d has no '=': %q", n, line))
			continue
		}
		key, val = strings.TrimSpace(key), strings.TrimSpace(val)
		if len(val) >= 2 && (val[0] == '"' || val[0] == '\'') && val[len(val)-1] == val[0] {
			val = val[1 : len(val)-1]
		} else if i := strings.Index(val, " #"); i >= 0 {
			val = strings.TrimSpace(val[:i])
		}
		if _, dup := ef.values[key]; dup {
			ef.issues = append(ef.issues, fmt.Sprintf("%s is set more than once; the last one wins", key))
		} else {
			ef.order = append(ef.order, key)
		}
		ef.values[key] = val
	}
	return ef, sc.Err()
}

func (e *envFile) get(key, def string) string {
	if v := e.values[key]; v != "" {
		return v
	}
	return def
}

var knownKeys = map[string]bool{
	"ADMIN_TOKEN": true, "BIND_ADDR": true, "PORT": true,
	"CF_PROTOCOL": true, "CF_EXTRA_ARGS": true,
	"CF_API_TOKEN": true, "CF_DOMAIN": true, "CF_TUNNEL_NAME": true,
	"LOG_MAX_SIZE_MB": true, "LOG_RETENTION_DAYS": true, "LOG_MAX_ARCHIVES": true,
	"LOG_LEVEL": true, "RECENT_REQUESTS": true,
}
