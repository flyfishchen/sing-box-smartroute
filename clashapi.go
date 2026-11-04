package smartroute

import (
	"context"
	_ "embed"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"time"
	"unsafe"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/render"
	"github.com/sagernet/cors"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/common/urltest"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/experimental"
	"github.com/sagernet/sing-box/experimental/clashapi"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing-box/protocol/group"
	N "github.com/sagernet/sing/common/network"
	"github.com/sagernet/sing/service"
)

//go:embed smartroute_ui.html
var smartRouteDashboardHTML []byte

func init() {
	experimental.RegisterClashServerConstructor(NewClashServer)
}

type smartRouteOverviewResponse struct {
	Timestamp time.Time               `json:"timestamp"`
	Version   string                  `json:"version"`
	Groups    []SmartRouteGroupStatus `json:"groups"`
}

func NewClashServer(ctx context.Context, logFactory log.ObservableFactory, options option.ClashAPIOptions) (adapter.LifecycleService, error) {
	baseServer, err := clashapi.NewServer(ctx, logFactory, options)
	if err != nil {
		return nil, err
	}
	if httpSrv := extractClashHTTPServer(baseServer); httpSrv != nil && httpSrv.Handler != nil {
		httpSrv.Handler = wrapClashHTTPHandler(ctx, httpSrv.Handler, options)
	}
	return baseServer, nil
}

func extractClashHTTPServer(server adapter.LifecycleService) *http.Server {
	val := reflect.ValueOf(server)
	if val.Kind() != reflect.Ptr || val.Elem().Kind() != reflect.Struct {
		return nil
	}
	field := val.Elem().FieldByName("httpServer")
	if !field.IsValid() || !field.CanAddr() {
		return nil
	}
	httpSrv, ok := reflect.NewAt(field.Type(), unsafe.Pointer(field.UnsafeAddr())).Elem().Interface().(*http.Server)
	if !ok {
		return nil
	}
	return httpSrv
}

func wrapClashHTTPHandler(ctx context.Context, baseHandler http.Handler, options option.ClashAPIOptions) http.Handler {
	outboundManager := service.FromContext[adapter.OutboundManager](ctx)
	metrics := MetricsFromContext(ctx)

	chiRouter := chi.NewRouter()
	allowedOrigins := options.AccessControlAllowOrigin
	if len(allowedOrigins) == 0 {
		allowedOrigins = []string{"*"}
	}
	corsMiddleware := cors.New(cors.Options{
		AllowedOrigins:      allowedOrigins,
		AllowedMethods:      []string{"GET", "POST", "PUT", "PATCH", "DELETE"},
		AllowedHeaders:      []string{"Content-Type", "Authorization"},
		AllowPrivateNetwork: options.AccessControlAllowPrivateNetwork,
		MaxAge:              300,
	})
	chiRouter.Use(corsMiddleware.Handler)
	chiRouter.Get("/smartroute", serveSmartRouteDashboard)
	chiRouter.Get("/smartroute/", serveSmartRouteDashboard)
	chiRouter.Group(func(r chi.Router) {
		r.Use(clashAuthentication(options.Secret))
		r.Mount("/smartroute/api", smartRouteAPIRouter(outboundManager, metrics))
	})

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if path == "/smartroute" || path == "/smartroute/" || strings.HasPrefix(path, "/smartroute/api/") || path == "/smartroute/api" {
			chiRouter.ServeHTTP(w, r)
			return
		}
		baseHandler.ServeHTTP(w, r)
	})
}

func clashAuthentication(serverSecret string) func(next http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if serverSecret == "" {
				next.ServeHTTP(w, r)
				return
			}
			if r.Header.Get("Upgrade") == "websocket" && r.URL.Query().Get("token") != "" {
				if r.URL.Query().Get("token") != serverSecret {
					render.Status(r, http.StatusUnauthorized)
					render.JSON(w, r, render.M{"message": "Unauthorized"})
					return
				}
				next.ServeHTTP(w, r)
				return
			}
			header := r.Header.Get("Authorization")
			bearer, token, found := strings.Cut(header, " ")
			if bearer != "Bearer" || !found || token != serverSecret {
				render.Status(r, http.StatusUnauthorized)
				render.JSON(w, r, render.M{"message": "Unauthorized"})
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func serveSmartRouteDashboard(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(smartRouteDashboardHTML)
}

func smartRouteAPIRouter(outboundManager adapter.OutboundManager, metrics *MetricsStorage) http.Handler {
	r := chi.NewRouter()
	r.Get("/overview", getSmartRouteOverview(outboundManager))
	r.Post("/groups/{name}/probe", probeSmartRouteGroup(outboundManager))
	r.Put("/groups/{name}/config", updateSmartRouteGroupConfig(outboundManager))
	r.Post("/nodes/{name}/reset", resetSmartRouteNode(outboundManager, metrics))
	r.Post("/nodes/{name}/probe", probeSmartRouteNode(outboundManager, metrics))
	return r
}

func getEscapeParam(r *http.Request, key string) string {
	param := chi.URLParam(r, key)
	if unescaped, err := url.PathUnescape(param); err == nil {
		return unescaped
	}
	return param
}

func getSmartRouteOverview(outboundManager adapter.OutboundManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var groups []SmartRouteGroupStatus
		if outboundManager != nil {
			for _, detour := range outboundManager.Outbounds() {
				if ut, ok := detour.(*URLTest); ok {
					groups = append(groups, ut.SmartRouteStatus())
				}
			}
		}
		render.JSON(w, r, smartRouteOverviewResponse{
			Timestamp: time.Now(),
			Version:   C.Version,
			Groups:    groups,
		})
	}
}

func probeSmartRouteGroup(outboundManager adapter.OutboundManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := getEscapeParam(r, "name")
		detour, loaded := outboundManager.Outbound(name)
		if !loaded {
			render.Status(r, http.StatusNotFound)
			render.JSON(w, r, render.M{"message": "resource not found"})
			return
		}
		ut, ok := detour.(*URLTest)
		if !ok {
			render.Status(r, http.StatusBadRequest)
			render.JSON(w, r, render.M{"message": "outbound is not a urltest/smartroute group"})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), C.TCPTimeout)
		defer cancel()
		_, err := ut.URLTest(ctx)
		if err != nil {
			render.Status(r, http.StatusInternalServerError)
			render.JSON(w, r, render.M{"message": err.Error()})
			return
		}
		render.JSON(w, r, ut.SmartRouteStatus())
	}
}

func updateSmartRouteGroupConfig(outboundManager adapter.OutboundManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := getEscapeParam(r, "name")
		detour, loaded := outboundManager.Outbound(name)
		if !loaded {
			render.Status(r, http.StatusNotFound)
			render.JSON(w, r, render.M{"message": "resource not found"})
			return
		}
		ut, ok := detour.(*URLTest)
		if !ok {
			render.Status(r, http.StatusBadRequest)
			render.JSON(w, r, render.M{"message": "outbound is not a urltest/smartroute group"})
			return
		}
		var dto SmartRouteGroupConfigDTO
		if err := render.DecodeJSON(r.Body, &dto); err != nil {
			render.Status(r, http.StatusBadRequest)
			render.JSON(w, r, render.M{"message": err.Error()})
			return
		}
		if err := ut.UpdateSmartRouteConfig(dto); err != nil {
			render.Status(r, http.StatusBadRequest)
			render.JSON(w, r, render.M{"message": err.Error()})
			return
		}
		render.JSON(w, r, ut.SmartRouteStatus())
	}
}

func resetSmartRouteNode(outboundManager adapter.OutboundManager, metrics *MetricsStorage) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := getEscapeParam(r, "name")
		proxy, loaded := outboundManager.Outbound(name)
		if !loaded {
			render.Status(r, http.StatusNotFound)
			render.JSON(w, r, render.M{"message": "resource not found"})
			return
		}
		realTag := group.RealTag(proxy, N.NetworkTCP)
		metrics.ResetLinkMetrics(realTag)
		for _, detour := range outboundManager.Outbounds() {
			if ut, ok := detour.(interface{ PerformUpdateCheck() }); ok {
				ut.PerformUpdateCheck()
			}
		}
		render.NoContent(w, r)
	}
}

func probeSmartRouteNode(outboundManager adapter.OutboundManager, metrics *MetricsStorage) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := getEscapeParam(r, "name")
		proxy, loaded := outboundManager.Outbound(name)
		if !loaded {
			render.Status(r, http.StatusNotFound)
			render.JSON(w, r, render.M{"message": "resource not found"})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), C.TCPTimeout)
		defer cancel()
		delay, err := urltest.URLTest(ctx, DefaultURLTestURL, proxy)
		realTag := group.RealTag(proxy, N.NetworkTCP)
		if err != nil && r.Context().Err() == nil {
			if snap, ok := metrics.LoadLinkMetrics(realTag); ok && snap.Available {
				retryCtx, retryCancel := context.WithTimeout(r.Context(), 3*time.Second)
				retryDelay, retryErr := urltest.URLTest(retryCtx, DefaultURLTestURL, proxy)
				retryCancel()
				if retryErr == nil {
					delay = retryDelay
					err = nil
				}
			}
		}
		if err != nil {
			metrics.RecordProbeFailure(realTag)
		} else {
			delay = metrics.RecordProbeSuccess(realTag, delay)
		}
		for _, detour := range outboundManager.Outbounds() {
			if ut, ok := detour.(interface{ PerformUpdateCheck() }); ok {
				ut.PerformUpdateCheck()
			}
		}
		if err != nil {
			render.Status(r, http.StatusServiceUnavailable)
			render.JSON(w, r, render.M{"message": err.Error()})
			return
		}
		render.JSON(w, r, render.M{"delay": delay})
	}
}
