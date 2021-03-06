package chartmuseum

import (
	"context"
	"fmt"
	"regexp"
	"sync"
	"time"

	"github.com/kubernetes-helm/chartmuseum/pkg/repo"
	"github.com/kubernetes-helm/chartmuseum/pkg/storage"

	"github.com/gin-gonic/gin"
	"github.com/zsais/go-gin-prometheus"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	helm_repo "k8s.io/helm/pkg/repo"
)

type (
	// Logger handles all logging from application
	Logger struct {
		*zap.SugaredLogger
	}

	// Router handles all incoming HTTP requests
	Router struct {
		*gin.Engine
	}

	// Server contains a Logger, Router, storage backend and object cache
	Server struct {
		Logger                 *Logger
		Router                 *Router
		RepositoryIndex        *repo.Index
		StorageBackend         storage.Backend
		StorageCache           []storage.Object
		StorageCacheLock       *sync.Mutex
		AllowOverwrite         bool
		TlsCert                string
		TlsKey                 string
		ChartPostFormFieldName string
		ProvPostFormFieldName  string
	}

	// ServerOptions are options for constructing a Server
	ServerOptions struct {
		StorageBackend         storage.Backend
		LogJSON                bool
		Debug                  bool
		EnableAPI              bool
		AllowOverwrite         bool
		EnableMetrics          bool
		ChartURL               string
		TlsCert                string
		TlsKey                 string
		Username               string
		Password               string
		ChartPostFormFieldName string
		ProvPostFormFieldName  string
	}
)

// NewLogger creates a new Logger instance
func NewLogger(json bool, debug bool) (*Logger, error) {
	config := zap.NewDevelopmentConfig()
	config.DisableStacktrace = true
	config.Development = false
	if json {
		config.Encoding = "json"
	} else {
		config.EncoderConfig.EncodeLevel = zapcore.CapitalColorLevelEncoder
	}
	if !debug {
		config.Level = zap.NewAtomicLevelAt(zap.InfoLevel)
	}
	logger, err := config.Build()
	if err != nil {
		return new(Logger), err
	}
	defer logger.Sync()
	return &Logger{logger.Sugar()}, nil
}

func mapURLWithParamsBackToRouteTemplate(c *gin.Context) string {
	url := c.Request.URL.String()
	for _, p := range c.Params {
		re := regexp.MustCompile(fmt.Sprintf(`(^.*?)/\b%s\b(.*$)`, regexp.QuoteMeta(p.Value)))
		url = re.ReplaceAllString(url, fmt.Sprintf(`$1/:%s$2`, p.Key))
	}
	return url
}

// NewRouter creates a new Router instance
func NewRouter(logger *Logger, username string, password string, enableMetrics bool) *Router {
	gin.SetMode(gin.ReleaseMode)
	engine := gin.New()
	engine.Use(loggingMiddleware(logger), gin.Recovery())
	if username != "" && password != "" {
		users := make(map[string]string)
		users[username] = password
		engine.Use(gin.BasicAuthForRealm(users, "ChartMuseum"))
	}
	if enableMetrics {
		p := ginprometheus.NewPrometheus("chartmuseum")
		// For every route containing parameters (e.g. `/charts/:filename`, `/api/charts/:name/:version`, etc)
		// the actual parameter values will be replaced by their name, to minimize the cardinality of the
		// `chartmuseum_requests_total{url=..}` Prometheus counter.
		p.ReqCntURLLabelMappingFn = mapURLWithParamsBackToRouteTemplate
		p.Use(engine)
	}
	return &Router{engine}
}

// NewServer creates a new Server instance
func NewServer(options ServerOptions) (*Server, error) {
	logger, err := NewLogger(options.LogJSON, options.Debug)
	if err != nil {
		return new(Server), nil
	}

	router := NewRouter(logger, options.Username, options.Password, options.EnableMetrics)

	server := &Server{
		Logger:                 logger,
		Router:                 router,
		RepositoryIndex:        repo.NewIndex(options.ChartURL),
		StorageBackend:         options.StorageBackend,
		StorageCache:           []storage.Object{},
		StorageCacheLock:       &sync.Mutex{},
		AllowOverwrite:         options.AllowOverwrite,
		TlsCert:                options.TlsCert,
		TlsKey:                 options.TlsKey,
		ChartPostFormFieldName: options.ChartPostFormFieldName,
		ProvPostFormFieldName:  options.ProvPostFormFieldName,
	}

	server.setRoutes(options.EnableAPI)

	err = server.regenerateRepositoryIndex()
	return server, err
}

// Listen starts server on a given port
func (server *Server) Listen(port int) {
	server.Logger.Infow("Starting ChartMuseum",
		"port", port,
	)
	if server.TlsCert != "" && server.TlsKey != "" {
		server.Logger.Fatal(server.Router.RunTLS(fmt.Sprintf(":%d", port), server.TlsCert, server.TlsKey))
	} else {
		server.Logger.Fatal(server.Router.Run(fmt.Sprintf(":%d", port)))
	}
}

func loggingMiddleware(logger *Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()

		msg := "Request served"
		status := c.Writer.Status()

		meta := []interface{}{
			"path", c.Request.URL.Path,
			"comment", c.Errors.ByType(gin.ErrorTypePrivate).String(),
			"latency", time.Now().Sub(start),
			"clientIP", c.ClientIP(),
			"method", c.Request.Method,
			"statusCode", status,
		}

		switch {
		case status == 200 || status == 201:
			logger.Infow(msg, meta...)
		case status == 404:
			logger.Warnw(msg, meta...)
		default:
			logger.Errorw(msg, meta...)
		}
	}
}

func (server *Server) syncRepositoryIndex() error {
	_, diff, err := server.listObjectsGetDiff()
	if err != nil {
		return err
	}
	if !diff.Change {
		return nil
	}
	err = server.regenerateRepositoryIndex()
	return err
}

func (server *Server) listObjectsGetDiff() ([]storage.Object, storage.ObjectSliceDiff, error) {
	allObjects, err := server.StorageBackend.ListObjects()
	if err != nil {
		return []storage.Object{}, storage.ObjectSliceDiff{}, err
	}

	// filter out storage objects that dont have extension used for chart packages (.tgz)
	filteredObjects := []storage.Object{}
	for _, object := range allObjects {
		if object.HasExtension(repo.ChartPackageFileExtension) {
			filteredObjects = append(filteredObjects, object)
		}
	}

	diff := storage.GetObjectSliceDiff(server.StorageCache, filteredObjects)
	return filteredObjects, diff, nil
}

func (server *Server) regenerateRepositoryIndex() error {
	server.Logger.Debugw("Acquiring storage cache lock")
	server.StorageCacheLock.Lock()
	server.Logger.Debugw("Storage cache lock acquired")
	defer func() {
		server.Logger.Debugw("Releasing storage cache lock")
		server.StorageCacheLock.Unlock()
	}()

	objects, diff, err := server.listObjectsGetDiff()
	if err != nil {
		return err
	}

	index := &repo.Index{
		IndexFile: server.RepositoryIndex.IndexFile,
		Raw:       server.RepositoryIndex.Raw,
		ChartURL:  server.RepositoryIndex.ChartURL,
	}

	for _, object := range diff.Removed {
		err := server.removeIndexObject(index, object)
		if err != nil {
			return err
		}
	}

	for _, object := range diff.Updated {
		err := server.updateIndexObject(index, object)
		if err != nil {
			return err
		}
	}

	// Parallelize retrieval of added objects to improve startup speed
	err = server.addIndexObjectsAsync(index, diff.Added)
	if err != nil {
		return err
	}

	server.Logger.Debug("Regenerating index.yaml")
	err = index.Regenerate()
	if err != nil {
		return err
	}

	server.RepositoryIndex = index
	server.StorageCache = objects
	return nil
}

func (server *Server) removeIndexObject(index *repo.Index, object storage.Object) error {
	chartVersion, err := server.getObjectChartVersion(object, false)
	if err != nil {
		return server.checkInvalidChartPackageError(object, err, "removed")
	}
	server.Logger.Debugw("Removing chart from index",
		"name", chartVersion.Name,
		"version", chartVersion.Version,
	)
	index.RemoveEntry(chartVersion)
	return nil
}

func (server *Server) updateIndexObject(index *repo.Index, object storage.Object) error {
	chartVersion, err := server.getObjectChartVersion(object, true)
	if err != nil {
		return server.checkInvalidChartPackageError(object, err, "updated")
	}
	server.Logger.Debugw("Updating chart in index",
		"name", chartVersion.Name,
		"version", chartVersion.Version,
	)
	index.UpdateEntry(chartVersion)
	return nil
}

func (server *Server) addIndexObjectsAsync(index *repo.Index, objects []storage.Object) error {
	numObjects := len(objects)
	if numObjects == 0 {
		return nil
	}

	server.Logger.Debugw("Loading charts packages from storage (this could take awhile)",
		"total", numObjects,
	)

	type cvResult struct {
		cv  *helm_repo.ChartVersion
		err error
	}

	cvChan := make(chan cvResult)

	// Provide a mechanism to short-circuit object downloads in case of error
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	for _, object := range objects {
		go func(o storage.Object) {
			select {
			case <-ctx.Done():
				return
			default:
				chartVersion, err := server.getObjectChartVersion(o, true)
				if err != nil {
					err = server.checkInvalidChartPackageError(o, err, "added")
				}
				if err != nil {
					cancel()
				}
				cvChan <- cvResult{chartVersion, err}
			}
		}(object)
	}

	for validCount := 0; validCount < numObjects; validCount++ {
		cvRes := <-cvChan
		if cvRes.err != nil {
			return cvRes.err
		}
		if cvRes.cv == nil {
			continue
		}
		server.Logger.Debugw("Adding chart to index",
			"name", cvRes.cv.Name,
			"version", cvRes.cv.Version,
		)
		index.AddEntry(cvRes.cv)
	}

	return nil
}

func (server *Server) getObjectChartVersion(object storage.Object, load bool) (*helm_repo.ChartVersion, error) {
	if load {
		var err error
		object, err = server.StorageBackend.GetObject(object.Path)
		if err != nil {
			return nil, err
		}
		if len(object.Content) == 0 {
			return nil, repo.ErrorInvalidChartPackage
		}
	}
	return repo.ChartVersionFromStorageObject(object)
}

func (server *Server) checkInvalidChartPackageError(object storage.Object, err error, action string) error {
	if err == repo.ErrorInvalidChartPackage {
		server.Logger.Warnw("Invalid package in storage",
			"action", action,
			"package", object.Path,
		)
		return nil
	}
	return err
}
