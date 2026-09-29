package server

import (
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/navidrome/navidrome/conf"
	"github.com/navidrome/navidrome/conf/configtest"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("root API compatibility routes", func() {
	BeforeEach(func() {
		DeferCleanup(configtest.SetupConfig())
		conf.Server.BasePath = ""
	})

	setupRootCompatServer := func(prefixes ...string) (*Server, *bool, *string, *string) {
		router := chi.NewRouter()
		server := &Server{router: router, appRoot: "/ui"}
		server.mountRootRedirector()

		called := false
		seenPath := ""
		seenQuery := ""
		child := chi.NewRouter()
		child.Post("/*", func(w http.ResponseWriter, r *http.Request) {
			called = true
			seenPath = r.URL.Path
			seenQuery = r.URL.Query().Get("x")
			// A root compatibility dispatch must not reuse the parent wildcard
			// context. The child mux owns this route context.
			Expect(chi.RouteContext(r.Context()).Routes).To(Equal(child))
			w.WriteHeader(http.StatusCreated)
		})
		server.MountRouterWithRootPrefixes("test API", "/jellyfin", child, prefixes...)
		return server, &called, &seenPath, &seenQuery
	}

	It("dispatches a root POST to the child router", func() {
		server, called, seenPath, _ := setupRootCompatServer("users")

		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/Users/AuthenticateByName", strings.NewReader(`{}`))
		server.router.ServeHTTP(w, r)

		Expect(w.Code).To(Equal(http.StatusCreated))
		Expect(*called).To(BeTrue())
		Expect(*seenPath).To(Equal("/Users/AuthenticateByName"))
	})

	It("strips BasePath before dispatching and preserves query parameters", func() {
		conf.Server.BasePath = "/music"
		server, called, seenPath, seenQuery := setupRootCompatServer("users")

		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/music/users/authenticatebyname?x=1", strings.NewReader(`{}`))
		server.router.ServeHTTP(w, r)

		Expect(w.Code).To(Equal(http.StatusCreated))
		Expect(*called).To(BeTrue())
		Expect(*seenPath).To(Equal("/users/authenticatebyname"))
		Expect(*seenQuery).To(Equal("1"))
	})

	It("does not consume an unrelated root API path", func() {
		server, called, _, _ := setupRootCompatServer("users")
		api := chi.NewRouter()
		api.Get("/health", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
		server.MountRouter("native API", "/api", api)

		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/api/health", nil)
		server.router.ServeHTTP(w, r)

		Expect(w.Code).To(Equal(http.StatusNoContent))
		Expect(*called).To(BeFalse())
	})

	It("requires a prefix segment boundary", func() {
		server, called, _, _ := setupRootCompatServer("users")

		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/users2/authenticatebyname", strings.NewReader(`{}`))
		server.router.ServeHTTP(w, r)

		Expect(w.Code).To(Equal(http.StatusMethodNotAllowed))
		Expect(*called).To(BeFalse())
	})
})
