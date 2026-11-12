package main

import (
	"bytes"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A device subscribes through the app API, a slot gets confirmed, and an encrypted
// push request reaches the device's push service.
func TestPushEndToEnd(t *testing.T) {
	got := make(chan *http.Request, 1)
	pushSvc := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- r
		w.WriteHeader(201)
	}))
	defer pushSvc.Close()

	alerts := NewAlerts(OpenKV(t.TempDir()))
	p, err := NewPush(OpenKV(t.TempDir()), "https://example.test", alerts)
	if err != nil {
		t.Fatal(err)
	}
	p.client = pushSvc.Client() // trust the test TLS cert
	priv, _ := ecdh.P256().GenerateKey(rand.Reader)
	auth := make([]byte, 16)
	rand.Read(auth)
	enc := base64.RawURLEncoding.EncodeToString
	body, _ := json.Marshal(map[string]any{"cc": "IN", "visa": "F1/M1", "city": "mumbai",
		"sub": map[string]any{"endpoint": pushSvc.URL + "/device1",
			"keys": map[string]string{"p256dh": enc(priv.PublicKey().Bytes()), "auth": enc(auth)}}})
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("POST", "/api/push/watch", bytes.NewReader(body)))
	if rec.Code != 200 {
		t.Fatalf("watch: %d %s", rec.Code, rec.Body)
	}
	alerts.match([]Row{{Key: rowKey("IN", "Mumbai", "F1/M1", "Regular"), CC: "IN", City: "Mumbai", Visa: "F1/M1",
		Open: 2, Earliest: future(12), Confirmed: true}})
	select {
	case r := <-got:
		if r.URL.Path != "/device1" || r.Header.Get("Content-Encoding") != "aes128gcm" || r.Header.Get("Authorization") == "" {
			t.Fatalf("push request looks wrong: %s %v", r.URL.Path, r.Header)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no push sent")
	}
}
