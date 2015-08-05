package main

import (
	"encoding/json"
	"errors"
	"fmt"
	stdlog "log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/go-kit/kit/addsvc/reqrep"
	"github.com/go-kit/kit/endpoint"
	kitlog "github.com/go-kit/kit/log"
	httptransport "github.com/go-kit/kit/transport/http"
	"golang.org/x/net/context"
)

// AddRequest is a request for the add method.
type AddRequest struct {
	A int64 `json:"a"`
	B int64 `json:"b"`
}

// AddResponse is a response to the add method.
type AddResponse struct {
	V int64 `json:"v"`
}

type Add func(context.Context, int64, int64) int64

func pureAdd(_ context.Context, a, b int64) int64 { return a + b }

func makeEndpoint(a Add) endpoint.Endpoint {
	return func(ctx context.Context, request interface{}) (interface{}, error) {
		select {
		default:
		case <-ctx.Done():
			return nil, endpoint.ErrContextCanceled
		}

		addReq, ok := request.(reqrep.AddRequest)
		if !ok {
			return nil, endpoint.ErrBadCast
		}

		v := a(ctx, addReq.A, addReq.B)
		return reqrep.AddResponse{V: v}, nil
	}
}

func authorizeMW(validUser string) endpoint.Middleware {
	return func(next endpoint.Endpoint) endpoint.Endpoint {
		return func(ctx context.Context, request interface{}) (interface{}, error) {
			auth := ctx.Value("User").(*BasicAuth)
			if auth.Username == validUser {
				return next(ctx, request)
			}
			return nil, errors.New("user not authorized")
		}
	}
}

func authenticateMW() endpoint.Middleware {
	return func(next endpoint.Endpoint) endpoint.Endpoint {
		return func(ctx context.Context, request interface{}) (interface{}, error) {
			auth := ctx.Value("User").(*BasicAuth)
			if auth.Authenticated() {
				return next(ctx, request)
			}
			return nil, errors.New("Bad credentials")
		}
	}
}

type BasicAuth struct {
	Username string
	Password string
	Ok       bool
}

// authenticated if username == password
func (ba *BasicAuth) Authenticated() bool {
	return ba.Ok && ba.Username == ba.Password
}

func authorizeBefore(ctx context.Context, r *http.Request) context.Context {
	u, p, ok := r.BasicAuth()
	ba := &BasicAuth{u, p, ok}

	return context.WithValue(ctx, "User", ba)
}

func makeHTTPBinding(ctx context.Context, e endpoint.Endpoint, before []httptransport.BeforeFunc, after []httptransport.AfterFunc) http.Handler {
	decode := func(r *http.Request) (interface{}, error) {
		var request reqrep.AddRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			return nil, err
		}
		r.Body.Close()
		return request, nil
	}
	encode := func(w http.ResponseWriter, response interface{}) error {
		return json.NewEncoder(w).Encode(response)
	}
	return httptransport.Server{
		Context:    ctx,
		Endpoint:   e,
		DecodeFunc: decode,
		EncodeFunc: encode,
		Before:     before,
		After:      append([]httptransport.AfterFunc{httptransport.SetContentType("application/json; charset=utf-8")}, after...),
	}
}

func main() {

	var logger kitlog.Logger
	logger = kitlog.NewLogfmtLogger(os.Stderr)
	logger = kitlog.NewContext(logger).With("ts", kitlog.DefaultTimestampUTC)
	stdlog.SetOutput(kitlog.NewStdlibAdapter(logger)) // redirect stdlib logging to us
	stdlog.SetFlags(0)                                // flags are handled in our logger
	debugAddr := ":8001"
	httpAddr := ":8000"
	root := context.Background()

	// Our business and operational domain
	var a Add = pureAdd
	//a = authorize()(a)

	// Server domain
	var e endpoint.Endpoint
	e = makeEndpoint(a)
	e = authenticateMW()(e)
	e = authorizeMW("user")(e)

	errc := make(chan error)
	go func() {
		errc <- interrupt()
	}()
	// Transport: HTTP (debug/instrumentation)
	go func() {
		logger.Log("addr", debugAddr, "transport", "debug")
		errc <- http.ListenAndServe(debugAddr, nil)
	}()
	// Transport: HTTP (JSON)
	go func() {
		ctx, cancel := context.WithCancel(root)
		defer cancel()
		before := []httptransport.BeforeFunc{authorizeBefore}
		after := []httptransport.AfterFunc{}
		handler := makeHTTPBinding(ctx, e, before, after)
		logger.Log("addr", httpAddr, "transport", "HTTP/JSON")
		errc <- http.ListenAndServe(httpAddr, handler)
	}()
	logger.Log("fatal", <-errc)
}

func interrupt() error {
	c := make(chan os.Signal)
	signal.Notify(c, syscall.SIGINT, syscall.SIGTERM)
	return fmt.Errorf("%s", <-c)
}
