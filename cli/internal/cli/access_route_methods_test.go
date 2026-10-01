package cli

import (
	"encoding/json"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
)

const (
	routeMethodAlias    = "served-alias"
	routeMethodHost     = "inference.example.test"
	routeMethodHTTP     = "HTTP"
	routeMethodChat     = "chat"
	routeMethodEndpoint = "endpoint"
)

type routeMethodCase struct {
	name    string
	methods []string
	action  string
	known   bool
	check   bool
	want    []string
	code    string
}

func TestAccessRouteMethodsByAction(t *testing.T) {
	for _, tc := range []routeMethodCase{
		{"POST-only display", []string{http.MethodPost}, routeMethodEndpoint, true, false, nil, ""},
		{"POST-only known chat", []string{http.MethodPost}, routeMethodChat, true, false, []string{"POST /models/v1/chat/completions"}, ""},
		{"POST-only check rejected", []string{http.MethodPost}, routeMethodEndpoint, true, true, nil, "UNSUPPORTED"},
		{"POST-only discovery chat rejected", []string{http.MethodPost}, routeMethodChat, false, false, nil, "UNSUPPORTED"},
		{"GET-only check", []string{http.MethodGet}, routeMethodEndpoint, true, true, []string{"GET /models/v1/models"}, ""},
		{"GET-only chat rejected", []string{http.MethodGet}, routeMethodChat, true, false, nil, "UNSUPPORTED"},
		{"unrestricted check", []string{""}, routeMethodEndpoint, true, true, []string{"GET /models/v1/models"}, ""},
		{"unrestricted known chat", []string{""}, routeMethodChat, true, false, []string{"POST /models/v1/chat/completions"}, ""},
		{"unrestricted discovery chat", []string{""}, routeMethodChat, false, false, []string{"GET /models/v1/models", "POST /models/v1/chat/completions"}, ""},
		{"split matches check selects GET", []string{http.MethodPost, http.MethodGet}, routeMethodEndpoint, true, true, []string{"GET /models/v1/models"}, ""},
		{"split matches known chat selects POST", []string{http.MethodGet, http.MethodPost}, routeMethodChat, true, false, []string{"POST /models/v1/chat/completions"}, ""},
		{"split matches discovery chat", []string{http.MethodGet, http.MethodPost}, routeMethodChat, false, false, []string{"GET /models/v1/models", "POST /models/v1/chat/completions"}, ""},
		{"GET with different headers cannot provide discovery", []string{http.MethodGet, http.MethodPost}, routeMethodChat, false, false, nil, "UNSUPPORTED"},
		{"HEAD-only check rejected", []string{http.MethodHead}, routeMethodEndpoint, true, true, nil, "UNSUPPORTED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runAccessRouteMethodCase(t, tc)
		})
	}
}

func runAccessRouteMethodCase(t *testing.T, tc routeMethodCase) {
	t.Helper()
	var mu sync.Mutex
	var requests []string
	server := accessTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, r.Method+" "+r.URL.Path)
		mu.Unlock()
		if r.Host != routeMethodHost || r.Header.Get("x-gateway-model-name") != routeMethodAlias {
			t.Error("lost route identity", r.Host, r.Header)
		}
		if r.Method == http.MethodGet {
			accessTestReply(w, Object{"data": accessTestObjects(Object{"id": routeMethodAlias})})
			return
		}
		var body Object
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["model"] != routeMethodAlias {
			t.Error("wrong served model", body, err)
		}
		accessTestReply(w, Object{"choices": accessTestObjects(Object{"message": Object{"content": "reply"}})})
	})
	model, gateway, route := routeMethodResources(tc, server.URL)
	client := &accessFakeClient{resources: []Object{model, gateway, route}}
	flags := Flags{"server": {server.URL + "/models"}}
	if tc.check {
		flags["check"] = []string{"true"}
	}
	if tc.action == routeMethodChat {
		flags["message"] = []string{"hello"}
	}
	c, _, _ := accessTestContext(client, flags)
	err := runAccess("model", tc.action, "llama", c)
	accessTestCode(t, err, tc.code)
	if tc.name == "POST-only discovery chat rejected" && err != nil && !strings.Contains(err.Error(), "discovery") {
		t.Fatalf("missing actionable discovery explanation: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(requests, tc.want) {
		t.Fatalf("HTTP requests = %v, want %v", requests, tc.want)
	}
}

func routeMethodResources(tc routeMethodCase, serverURL string) (Object, Object, Object) {
	model, gateway, route, _ := accessTestGateway()
	object(model["spec"])["gateway"] = Object{"httpRouteRef": "llama"}
	delete(object(route["metadata"]), "ownerReferences")
	if !tc.known {
		delete(object(get(model, "spec", "model")), "servedName")
		delete(object(get(model, "status", "gateway")), "modelName")
	}
	rule := objects(get(route, "spec", "rules"))[0]
	base := objects(rule["matches"])[0]
	matches := make([]any, 0, len(tc.methods))
	for _, method := range tc.methods {
		match := cloneObject(base)
		if method != "" {
			match["method"] = method
		}
		if tc.name == "GET with different headers cannot provide discovery" && method == http.MethodGet {
			match["headers"] = accessTestObjects(Object{"name": "x-gateway-model-name", "value": "other-model"})
		}
		matches = append(matches, match)
	}
	rule["matches"] = matches
	published, _ := url.Parse(serverURL)
	port, _ := strconv.Atoi(published.Port())
	listener := objects(get(gateway, "spec", "listeners"))[0]
	listener["protocol"], listener["port"] = routeMethodHTTP, port
	gateway["status"] = Object{"addresses": accessTestObjects(Object{"value": published.Hostname()})}
	return model, gateway, route
}
