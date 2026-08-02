package main

import (
	"log"
	"net/http"
	"net/http/httptest"

	"github.com/wu8685/ahsir/internal/ui"
)

func main() {
	f := newFixture()
	mock := httptest.NewServer(f.schedulerHandler())
	defer mock.Close()
	console, err := ui.New(mock.URL, "")
	if err != nil {
		log.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.Handle("/__test/", f.controlHandler())
	mux.Handle("/", console.Handler())
	server := &http.Server{Addr: "127.0.0.1:19809", Handler: requestLogger(mux)}
	log.Printf("UI E2E fixture listening on http://%s", server.Addr)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

type statusResponseWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (w *statusResponseWriter) Flush() {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *statusResponseWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.status = status
	w.wroteHeader = true
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusResponseWriter) Write(body []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(body)
}

func requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		logged := &statusResponseWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(logged, r)
		log.Printf("method=%s path=%s status=%d", r.Method, r.URL.RequestURI(), logged.status)
	})
}
