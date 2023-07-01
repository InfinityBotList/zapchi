// Defines a standard way to define routes
package uapi

import (
	"context"
	"io"
	"net/http"
	"reflect"
	"strings"
	"time"

	docs "github.com/infinitybotlist/eureka/doclib"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"github.com/go-chi/chi/v5"
	"github.com/go-playground/validator/v10"
	"golang.org/x/exp/slices"

	jsoniter "github.com/json-iterator/go"
)

type UAPIConstants struct {
	NotFound         string
	NotFoundPage     string
	BadRequest       string
	Forbidden        string
	Unauthorized     string
	InternalError    string
	MethodNotAllowed string
	BodyRequired     string
}

type UAPIErrorType interface {
	// Returns the error as the error type
	New(err string, ctx map[string]string) any
}

// Setup struct
type UAPIState struct {
	Logger              *zap.SugaredLogger
	Authorize           func(r Route, req *http.Request) (AuthData, HttpResponse, bool)
	AuthTypeMap         map[string]string // E.g. bot => Bot, user => User etc.
	RouteDataMiddleware func(rd *RouteData, req *http.Request) (*RouteData, error)

	// Used in cache algo
	Redis *redis.Client
	// Used in cache algo
	Context context.Context

	// Api constants
	Constants *UAPIConstants

	// UApi Error type to use for error responses
	UAPIErrorType UAPIErrorType
}

func SetupState(s UAPIState) {
	if s.Constants == nil {
		panic("Constants is nil")
	}

	state = s
}

var (
	json  = jsoniter.ConfigCompatibleWithStandardLibrary
	state UAPIState
)

// Stores the current tag
var CurrentTag string

// A API Router, not to be confused with Router which routes the actual routes
type APIRouter interface {
	Routes(r *chi.Mux)
	Tag() (string, string)
}

type Method int

const (
	GET Method = iota
	POST
	PATCH
	PUT
	DELETE
	HEAD
)

// Returns the method as a string
func (m Method) String() string {
	switch m {
	case GET:
		return "GET"
	case POST:
		return "POST"
	case PATCH:
		return "PATCH"
	case PUT:
		return "PUT"
	case DELETE:
		return "DELETE"
	case HEAD:
		return "HEAD"
	}

	panic("Invalid method")
}

type AuthType struct {
	URLVar       string
	Type         string
	AllowedScope string // If this is set, then ban checks are not fatal
}

type AuthData struct {
	TargetType string `json:"target_type"`
	ID         string `json:"id"`
	Authorized bool   `json:"authorized"`
	Banned     bool   `json:"banned"` // Only applicable with AllowedScope
}

// Represents a route on the API
type Route struct {
	Method       Method
	Pattern      string
	OpId         string
	Handler      func(d RouteData, r *http.Request) HttpResponse
	Setup        func()
	Docs         func() *docs.Doc
	Auth         []AuthType
	AuthOptional bool
}

type RouteData struct {
	Context context.Context
	Auth    AuthData
	Props   map[string]string // Stores additional properties
}

type Router interface {
	Get(pattern string, h http.HandlerFunc)
	Post(pattern string, h http.HandlerFunc)
	Patch(pattern string, h http.HandlerFunc)
	Put(pattern string, h http.HandlerFunc)
	Delete(pattern string, h http.HandlerFunc)
	Head(pattern string, h http.HandlerFunc)
}

func (r Route) String() string {
	return r.Method.String() + " " + r.Pattern + " (" + r.OpId + ")"
}

func (r Route) Route(ro Router) {
	if r.OpId == "" {
		panic("OpId is empty: " + r.String())
	}

	if r.Handler == nil {
		panic("Handler is nil: " + r.String())
	}

	if r.Docs == nil {
		panic("Docs is nil: " + r.String())
	}

	if r.Pattern == "" {
		panic("Pattern is empty: " + r.String())
	}

	if CurrentTag == "" {
		panic("CurrentTag is empty: " + r.String())
	}

	if r.Setup != nil {
		r.Setup()
	}

	docsObj := r.Docs()

	docsObj.Pattern = r.Pattern
	docsObj.OpId = r.OpId
	docsObj.Method = r.Method.String()
	docsObj.Tags = []string{CurrentTag}
	docsObj.AuthType = []string{}

	for _, auth := range r.Auth {
		t, ok := state.AuthTypeMap[auth.Type]

		if !ok {
			panic("Invalid auth type: " + auth.Type)
		}

		docsObj.AuthType = append(docsObj.AuthType, t)
	}

	// Count the number of { and } in the pattern
	brStart := strings.Count(r.Pattern, "{")
	brEnd := strings.Count(r.Pattern, "}")
	pathParams := []string{}
	patternParams := []string{}

	for _, param := range docsObj.Params {
		if param.In == "" || param.Name == "" || param.Schema == nil {
			panic("Param is missing required fields: " + r.String())
		}

		if param.In == "path" {
			pathParams = append(pathParams, param.Name)
		}
	}

	// Get pattern params from the pattern
	for _, param := range strings.Split(r.Pattern, "/") {
		if strings.HasPrefix(param, "{") && strings.HasSuffix(param, "}") {
			patternParams = append(patternParams, param[1:len(param)-1])
		} else if strings.Contains(param, "{") || strings.Contains(param, "}") {
			panic("{ and } in pattern but does not start with it " + r.String())
		}
	}

	if brStart != brEnd {
		panic("Mismatched { and } in pattern: " + r.String())
	}

	if brStart != len(pathParams) {
		panic("Mismatched number of params and { in pattern: " + r.String())
	}

	if !slices.Equal(patternParams, pathParams) {
		panic("Mismatched params in pattern and docs: " + r.String())
	}

	// Add the path params to the docs
	docs.Route(docsObj)

	handle := func(w http.ResponseWriter, req *http.Request) {
		ctx := req.Context()
		resp := make(chan HttpResponse)

		go func() {
			defer func() {
				err := recover()

				if err != nil {
					state.Logger.Error(err)
					resp <- HttpResponse{
						Status: http.StatusInternalServerError,
						Data:   state.Constants.InternalError,
					}
				}
			}()

			authData, httpResp, ok := state.Authorize(r, req)

			if !ok {
				resp <- httpResp
				return
			}

			rd := &RouteData{
				Context: ctx,
				Auth:    authData,
			}

			if state.RouteDataMiddleware != nil {
				var err error
				rd, err = state.RouteDataMiddleware(rd, req)

				if err != nil {
					resp <- HttpResponse{
						Status: http.StatusInternalServerError,
						Json:   state.UAPIErrorType.New(err.Error(), nil),
					}
					return
				}
			}

			resp <- r.Handler(*rd, req)
		}()

		respond(ctx, w, resp)
	}

	switch r.Method {
	case GET:
		ro.Get(r.Pattern, handle)
	case POST:
		ro.Post(r.Pattern, handle)
	case PATCH:
		ro.Patch(r.Pattern, handle)
	case PUT:
		ro.Put(r.Pattern, handle)
	case DELETE:
		ro.Delete(r.Pattern, handle)
	case HEAD:
		ro.Head(r.Pattern, handle)
	default:
		panic("Unknown method for route: " + r.String())
	}
}

func respond(ctx context.Context, w http.ResponseWriter, data chan HttpResponse) {
	select {
	case <-ctx.Done():
		return
	case msg, ok := <-data:
		if !ok {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(state.Constants.InternalError))
		}

		if msg.Redirect != "" {
			msg.Headers = map[string]string{
				"Location":     msg.Redirect,
				"Content-Type": "text/html; charset=utf-8",
			}
			msg.Data = "<a href=\"" + msg.Redirect + "\">Found</a>.\n"
			msg.Status = http.StatusFound
		}

		if len(msg.Headers) > 0 {
			for k, v := range msg.Headers {
				w.Header().Set(k, v)
			}
		}

		if msg.Json != nil {
			bytes, err := json.Marshal(msg.Json)

			if err != nil {
				state.Logger.Error(err)
				w.WriteHeader(http.StatusInternalServerError)
				w.Write([]byte(state.Constants.InternalError))
				return
			}

			// JSON needs this explicitly to avoid calling WriteHeader twice
			if msg.Status == 0 {
				w.WriteHeader(http.StatusOK)
			} else {
				w.WriteHeader(msg.Status)
			}

			w.Write(bytes)

			if msg.CacheKey != "" && msg.CacheTime.Seconds() > 0 {
				go func() {
					err := state.Redis.Set(state.Context, msg.CacheKey, bytes, msg.CacheTime).Err()

					if err != nil {
						state.Logger.Error(err)
					}
				}()
			}
		}

		if msg.Status == 0 {
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(msg.Status)
		}

		if len(msg.Bytes) > 0 {
			w.Write(msg.Bytes)
		}

		w.Write([]byte(msg.Data))
		return
	}
}

type HttpResponse struct {
	// Data is the data to be sent to the client
	Data string
	// Optional, can be used in place of Data
	Bytes []byte
	// Json body to be sent to the client
	Json any
	// Headers to set
	Headers map[string]string
	// Status is the HTTP status code to send
	Status int
	// Cache the JSON to redis
	CacheKey string
	// Duration to cache the JSON for
	CacheTime time.Duration
	// Redirect to a URL
	Redirect string
}

func CompileValidationErrors(payload any) map[string]string {
	var errors = make(map[string]string)

	structType := reflect.TypeOf(payload)

	for _, f := range reflect.VisibleFields(structType) {
		errors[f.Name] = f.Tag.Get("msg")

		arrayMsg := f.Tag.Get("amsg")

		if arrayMsg != "" {
			errors[f.Name+"$arr"] = arrayMsg
		}
	}

	return errors
}

func ValidatorErrorResponse(compiled map[string]string, v validator.ValidationErrors) HttpResponse {
	var errors = make(map[string]string)

	firstError := ""

	for i, err := range v {
		fname := err.StructField()
		if strings.Contains(err.Field(), "[") {
			// We have a array response, so we need to get the array name
			fname = strings.Split(err.Field(), "[")[0] + "$arr"
		}

		field := compiled[fname]

		var errorMsg string
		if field != "" {
			errorMsg = field + " [" + err.Tag() + "]"
		} else {
			errorMsg = err.Error()
		}

		if i == 0 {
			firstError = errorMsg
		}

		errors[err.StructField()] = errorMsg
	}

	return HttpResponse{
		Status: http.StatusBadRequest,
		Json:   state.UAPIErrorType.New(firstError, errors),
	}
}

// Creates a default HTTP response based on the status code
// 200 is treated as 204 No Content
func DefaultResponse(statusCode int) HttpResponse {
	switch statusCode {
	case http.StatusForbidden:
		return HttpResponse{
			Status: statusCode,
			Data:   state.Constants.Forbidden,
		}
	case http.StatusUnauthorized:
		return HttpResponse{
			Status: statusCode,
			Data:   state.Constants.Unauthorized,
		}
	case http.StatusNotFound:
		return HttpResponse{
			Status: statusCode,
			Data:   state.Constants.NotFound,
		}
	case http.StatusBadRequest:
		return HttpResponse{
			Status: statusCode,
			Data:   state.Constants.BadRequest,
		}
	case http.StatusInternalServerError:
		return HttpResponse{
			Status: statusCode,
			Data:   state.Constants.InternalError,
		}
	case http.StatusMethodNotAllowed:
		return HttpResponse{
			Status: statusCode,
			Data:   state.Constants.MethodNotAllowed,
		}
	case http.StatusNoContent, http.StatusOK:
		return HttpResponse{
			Status: http.StatusNoContent,
		}
	}

	return HttpResponse{
		Status: statusCode,
		Data:   state.Constants.InternalError,
	}
}

// Read body
func marshalReq(r *http.Request, dst interface{}) (resp HttpResponse, ok bool) {
	defer r.Body.Close()

	bodyBytes, err := io.ReadAll(r.Body)

	if err != nil {
		state.Logger.Error(err)
		return DefaultResponse(http.StatusInternalServerError), false
	}

	if len(bodyBytes) == 0 {
		return HttpResponse{
			Status: http.StatusBadRequest,
			Json:   state.Constants.BodyRequired,
		}, false
	}

	err = json.Unmarshal(bodyBytes, &dst)

	if err != nil {
		state.Logger.Error(err)
		return HttpResponse{
			Status: http.StatusBadRequest,
			Json: state.UAPIErrorType.New("Invalid JSON", map[string]string{
				"error": err.Error(),
			}),
		}, false
	}

	return HttpResponse{}, true
}

func MarshalReq(r *http.Request, dst any) (resp HttpResponse, ok bool) {
	return marshalReq(r, dst)
}

func MarshalReqWithHeaders(r *http.Request, dst any, headers map[string]string) (resp HttpResponse, ok bool) {
	resp, err := marshalReq(r, dst)

	resp.Headers = headers

	return resp, err
}
