package dashing

import (
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/gin-gonic/gin"
	"github.com/znly/go-dashing/dashingtypes"
	sprockets "github.com/znly/go-sprockets"
	gerb "gopkg.in/karlseguin/gerb.v0"
)

type serverError struct {
	Code          int
	InternalError error `json:"error"`
}

type CustomRoute struct{
	Path 			string
	HttpMethod 		string
	Handler 		gin.HandlerFunc
}

func (se *serverError) Error() string {
	return fmt.Sprintf("%d: %s", se.Code, se.InternalError.Error())
}

var (
	errNotFound            = &serverError{http.StatusNotFound, errors.New("not found")}
	errUnauthorized        = &serverError{http.StatusUnauthorized, errors.New("unauthorized")}
	errBadRequest          = &serverError{http.StatusBadRequest, errors.New("bad request")}
	errInternalserverError = &serverError{http.StatusInternalServerError, errors.New("internal server error")}
)

type server struct {
	dev                  bool
	webroot              string
	assetsPipeline       *sprockets.Sprocket
	broker               *broker
	defaultDashboardPath string
	defaultDashboard     string
	authToken            string
	hostbind             string
	r					*gin.Engine
	customRoutes		[]*CustomRoute
}

func upsertParam(key, value string, c *gin.Context) {
	for _, p := range c.Params {
		if p.Key == key {
			p.Value = value
			return
		}
	}
	c.Params = append(c.Params, gin.Param{Key: key, Value: value})
}

func (s *server) indexHandler(c *gin.Context) {
	files, _ := filepath.Glob(filepath.Join(s.webroot, "dashboards", "*.gerb"))
	if len(files) == 0 {
		s.abortWithError(c, errNotFound)
		return
	}
	sort.Strings(files)
	if files[sort.SearchStrings(files, s.defaultDashboardPath)] == s.defaultDashboardPath {
		upsertParam("dashboard", s.defaultDashboard, c)
		s.dashboardHandler(c)
		return
	}
	for _, file := range files {
		basename := filepath.Base(file)
		dashboard := basename[:len(basename)-5]
		if dashboard != "layout" {
			upsertParam("dashboard", s.defaultDashboard, c)
			s.dashboardHandler(c)
			return
		}
	}
	s.abortWithError(c, errNotFound)
}

func (s *server) eventsHandler(c *gin.Context) {
	f, ok := c.Writer.(http.Flusher)
	if !ok {
		log.Error("Streaming unsupported!")
		s.abortWithError(c, errInternalserverError)
		return
	}

	closeNotififier, ok := c.Writer.(http.CloseNotifier)
	if !ok {
		log.Error("Close notification unsupported!")
		s.abortWithError(c, errInternalserverError)
		return
	}
	events := make(chan *dashingtypes.Event)
	s.broker.newClients <- events
	defer func() {
		s.broker.defunctClients <- events
	}()

	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")
	c.Writer.Header().Set("X-Accel-Buffering", "no")
	closer := closeNotififier.CloseNotify()

	for {
		select {
		case event := <-events:
			data := make(map[string]interface{})
			for k, v := range event.Body {
				data[k] = v
			}
			data["id"] = event.ID
			data["updatedAt"] = int32(time.Now().Unix())
			json, err := json.Marshal(data)
			if err != nil {
				continue
			}
			if event.Target != "" {
				fmt.Fprintf(c.Writer, "event: %s\n", event.Target)
			}
			fmt.Fprintf(c.Writer, "data: %s\n\n", json)
			f.Flush()
		case <-closer:
			log.Println("Closing connection")
			return
		}
	}
}

func (s *server) dashboardHandler(c *gin.Context) {
	dashboard := c.Param("dashboard")
	if dashboard == "" {
		dashboard = fmt.Sprintf("events%s", c.Param("suffix"))
	} else if dashboard == "events" {
		s.eventsHandler(c)
		return
	}
	template, err := gerb.ParseFile(true, fmt.Sprintf("%s/dashboards/%s.gerb", s.webroot, dashboard), s.webroot+"dashboards/layout.gerb")
	if err != nil {
		log.WithError(err).Error("Dashboard Not Found")
		s.abortWithError(c, errNotFound)
		return
	}
	c.Writer.Header().Set("Content-Type", "text/html; charset=UTF-8")
	template.Render(c.Writer, map[string]interface{}{
		"dashboard":   dashboard,
		"development": s.dev,
		"request":     c.Request,
	})
}

func (s *server) checkDataAuth(data map[string]interface{}) bool {
	val, ok := data["auth_token"]
	if !ok {
		return true
	}
	if authToken, ok := val.(string); !ok || authToken != s.authToken {
		return true
	}
	data["auth_token"] = nil
	return false
}

func (s *server) dashboardEventHandler(c *gin.Context) {
	if c.Request.Body != nil {
		defer c.Request.Body.Close()
	}

	var data map[string]interface{}

	if err := json.NewDecoder(c.Request.Body).Decode(&data); err != nil {
		log.WithError(err).Error("Dashboard Event Handler cant decode JSON")
		s.abortWithError(c, errBadRequest)
		return
	}
	if s.checkDataAuth(data) {
		s.abortWithError(c, errUnauthorized)
		return
	}
	id := c.Param("id")
	data["dashboard"] = id
	s.broker.events <- &dashingtypes.Event{
		ID:     id,
		Body:   data,
		Target: "dashboards"}

	c.Writer.WriteHeader(http.StatusNoContent)
}

func (s *server) widgetHandler(c *gin.Context) {
	widget := c.Param("widget")
	widget = widget[0 : len(widget)-5]
	template, err := gerb.ParseFile(true, fmt.Sprintf("%s/widgets/%s/%s.html", s.webroot, widget, widget))

	if err != nil {
		log.WithError(err).Error("Widget Not Found")
		s.abortWithError(c, errNotFound)
		return
	}

	c.Writer.Header().Set("Content-Type", "text/html; charset=UTF-8")

	template.Render(c.Writer, nil)
}

func (s *server) widgetEventHandler(c *gin.Context) {
	if c.Request.Body != nil {
		defer c.Request.Body.Close()
	}

	var data map[string]interface{}

	if err := json.NewDecoder(c.Request.Body).Decode(&data); err != nil {
		log.WithError(err).Error("Widget Event Handler cant decode JSON")
		s.abortWithError(c, errBadRequest)
		return
	}
	if s.checkDataAuth(data) {
		s.abortWithError(c, errUnauthorized)
		return
	}
	s.broker.events <- &dashingtypes.Event{
		ID:     c.Param("id"),
		Body:   data,
		Target: ""}

	c.Writer.WriteHeader(http.StatusNoContent)
}

func (s *server) getAssets(c *gin.Context) {
	url := c.Param("any")
	for len(url) > 0 && strings.HasPrefix(url, "/") {
		url = url[1:]
	}
	content, err := s.assetsPipeline.GetAsset(url)
	if err != nil {
		log.WithError(err).Error("Problem with an asset")
		s.abortWithError(c, errInternalserverError)
		return
	}
	c.Header("Content-type", mime.TypeByExtension(filepath.Ext(url)))
	c.Status(http.StatusOK)
	c.Writer.Write(content)
}

func (s *server) ConfigureCustomRoutes(routes []*CustomRoute){
	s.customRoutes = routes
}

func (s *server) registerCustomRoutes(){
	r := s.r
	for _, route := range s.customRoutes{
		r.Handle(route.HttpMethod, route.Path, route.Handler)
	}
}

func (s *server) registerDefaultRoutes(){
	r := s.r
	r.GET("/", s.indexHandler)
	r.GET("/assets/*any", s.getAssets)
	r.GET("/events", s.eventsHandler)
	r.GET("/events:suffix", s.dashboardHandler) // workaround for router edge case
	r.GET("/dashboards/:dashboard", s.dashboardHandler)
	r.POST("/dashboards/:id", s.dashboardEventHandler)
	r.GET("/views/:widget", s.widgetHandler)
	r.POST("/widgets/:id", s.widgetEventHandler)
}

func (s *server) start() error {
	s.registerDefaultRoutes()
	s.registerCustomRoutes()
	return s.r.Run(s.hostbind)
}

func newServer(b *broker, webroot, dashingJSRoot, defaultDashboard, authToken, host, port string) (*server, error) {
	s, err := sprockets.NewWithDefault(webroot+"/assets", "")
	if err != nil {
		return nil, err
	}
	s.PushBackExtensionPath(".coffee", dashingJSRoot)
	s.PushBackExtensionPath(".js", dashingJSRoot)
	s.PushBackExtensionPath(".coffee", webroot+"/assets")
	s.PushBackExtensionPath(".js", webroot+"/assets")
	return &server{
		dev:                  false,
		webroot:              webroot,
		broker:               b,
		assetsPipeline:       s,
		defaultDashboard:     defaultDashboard,
		defaultDashboardPath: filepath.Join(webroot, "dashboards", defaultDashboard+".gerb"),
		authToken:            authToken,
		hostbind:             host + ":" + port,
		r: 					  gin.Default(),
	}, nil
}

func (s *server) abortWithError(c *gin.Context, err *serverError) {
	c.AbortWithError(err.Code, err.InternalError)
	c.JSON(err.Code, err)
}

func (s *server) getSliceInfo(c *gin.Context) (int, int, error) {
	offset, err := strconv.Atoi(c.Param("offset"))
	if err != nil {
		return -1, -1, err
	}
	limit, err := strconv.Atoi(c.Param("limit"))
	if err != nil {
		return -1, -1, err
	}
	return offset, limit, nil
}
