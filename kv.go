package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// KV is where alerts, push subscriptions and push keys are saved. Records are grouped
// by namespace ("watches", "push") and keyed by id. Only changed records are written.
//
// FileKV is used on a normal server with a disk. UpstashKV is used on hosts whose disk
// is wiped on restart (Render free): it keeps the data in a free Upstash Redis.
type KV interface {
	LoadAll(ns string) (map[string][]byte, error)
	Save(ns string, put map[string][]byte, del []string) error
}

func OpenKV(dataDir string) KV {
	if u, t := os.Getenv("UPSTASH_REDIS_REST_URL"), os.Getenv("UPSTASH_REDIS_REST_TOKEN"); u != "" && t != "" {
		return &UpstashKV{url: strings.TrimRight(u, "/"), token: t, http: &http.Client{Timeout: 15 * time.Second}}
	}
	return &FileKV{dir: dataDir, cache: map[string]map[string]json.RawMessage{}}
}

// ---------- disk ----------

type FileKV struct {
	dir   string
	mu    sync.Mutex
	cache map[string]map[string]json.RawMessage
}

func (f *FileKV) load(ns string) map[string]json.RawMessage {
	if m, ok := f.cache[ns]; ok {
		return m
	}
	m := map[string]json.RawMessage{}
	if b, err := os.ReadFile(filepath.Join(f.dir, ns+".json")); err == nil {
		json.Unmarshal(b, &m)
	}
	f.cache[ns] = m
	return m
}

func (f *FileKV) LoadAll(ns string) (map[string][]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string][]byte{}
	for k, v := range f.load(ns) {
		out[k] = v
	}
	return out, nil
}

func (f *FileKV) Save(ns string, put map[string][]byte, del []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	m := f.load(ns)
	for k, v := range put {
		m[k] = v
	}
	for _, k := range del {
		delete(m, k)
	}
	b, _ := json.Marshal(m)
	return writeFileAtomic(filepath.Join(f.dir, ns+".json"), b)
}

func writeFileAtomic(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ---------- Upstash Redis (REST) ----------

type UpstashKV struct {
	url, token string
	http       *http.Client
}

func (u *UpstashKV) do(path string, body any) ([]json.RawMessage, error) {
	j, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", u.url+path, bytes.NewReader(j))
	req.Header.Set("Authorization", "Bearer "+u.token)
	res, err := u.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return nil, fmt.Errorf("upstash: HTTP %d", res.StatusCode)
	}
	var out []struct {
		Result json.RawMessage `json:"result"`
		Error  string          `json:"error"`
	}
	if path == "/" { // single command returns one object
		var one struct {
			Result json.RawMessage `json:"result"`
			Error  string          `json:"error"`
		}
		if err := json.NewDecoder(res.Body).Decode(&one); err != nil {
			return nil, err
		}
		out = append(out, one)
	} else if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return nil, err
	}
	results := make([]json.RawMessage, len(out))
	for i, o := range out {
		if o.Error != "" {
			return nil, fmt.Errorf("upstash: %s", o.Error)
		}
		results[i] = o.Result
	}
	return results, nil
}

func (u *UpstashKV) LoadAll(ns string) (map[string][]byte, error) {
	r, err := u.do("/", []string{"HGETALL", "vs:" + ns})
	if err != nil {
		return nil, err
	}
	var flat []string
	if err := json.Unmarshal(r[0], &flat); err != nil {
		return nil, err
	}
	out := map[string][]byte{}
	for i := 0; i+1 < len(flat); i += 2 {
		out[flat[i]] = []byte(flat[i+1])
	}
	return out, nil
}

// Save sends all changes in one pipeline request, so a busy minute costs one call, not hundreds.
func (u *UpstashKV) Save(ns string, put map[string][]byte, del []string) error {
	var cmds [][]string
	if len(put) > 0 {
		c := []string{"HSET", "vs:" + ns}
		for k, v := range put {
			c = append(c, k, string(v))
		}
		cmds = append(cmds, c)
	}
	if len(del) > 0 {
		cmds = append(cmds, append([]string{"HDEL", "vs:" + ns}, del...))
	}
	if len(cmds) == 0 {
		return nil
	}
	_, err := u.do("/pipeline", cmds)
	return err
}
