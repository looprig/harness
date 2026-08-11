package serving_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/serve"
)

type emptyReader struct{}

func (emptyReader) ListSessions(context.Context, serve.Page) (serve.SessionList, error) {
	return serve.SessionList{Sessions: []serve.SessionSummary{}, Limit: 50, Done: true}, nil
}

func (emptyReader) ReadStatus(context.Context, uuid.UUID) (serve.SessionStatus, error) {
	return serve.SessionStatus{}, serve.SessionNotFoundError{}
}

func (emptyReader) ReadJournal(context.Context, uuid.UUID, serve.JournalPage) (serve.EventJournalPage, error) {
	return serve.EventJournalPage{}, serve.SessionNotFoundError{}
}

func Example_readOnlyHTTPAdapter() {
	handler := serve.ReadHandler(emptyReader{})
	request := httptest.NewRequest(http.MethodGet, "/v1/capabilities", http.NoBody)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	server, err := serve.Server("127.0.0.1:0", handler)
	if err != nil {
		panic(err)
	}
	fmt.Println(response.Code, response.Header().Get("Content-Type"), server.Addr)
	// Output:
	// 200 application/json 127.0.0.1:0
}
