package sippyserver

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/bigquery"
	"cloud.google.com/go/storage"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/push"

	"github.com/openshift/sippy/pkg/api/jobrunintervals"
	"github.com/openshift/sippy/pkg/apis/cache"
	"github.com/openshift/sippy/pkg/releasesync"

	"github.com/openshift/sippy/pkg/db/models"

	apitype "github.com/openshift/sippy/pkg/apis/api"
	"github.com/openshift/sippy/pkg/filter"
	"github.com/openshift/sippy/pkg/synthetictests"
	"github.com/openshift/sippy/pkg/util"

	log "github.com/sirupsen/logrus"

	"github.com/openshift/sippy/pkg/api"
	workloadmetricsv1 "github.com/openshift/sippy/pkg/apis/workloadmetrics/v1"
	"github.com/openshift/sippy/pkg/db"
	"github.com/openshift/sippy/pkg/db/query"
	"github.com/openshift/sippy/pkg/testidentification"
)

// Mode defines the server mode of operation, OpenShift or upstream Kubernetes.
type Mode string

const (
	ModeOpenShift  Mode = "openshift"
	ModeKubernetes Mode = "kube"
)

func NewServer(
	mode Mode,
	testGridLoadingConfig TestGridLoadingConfig,
	rawJobResultsAnalysisOptions RawJobResultsAnalysisConfig,
	displayDataOptions DisplayDataConfig,
	dashboardCoordinates []TestGridDashboardCoordinates,
	listenAddr string,
	syntheticTestManager synthetictests.SyntheticTestManager,
	variantManager testidentification.VariantManager,
	sippyNG fs.FS,
	static fs.FS,
	dbClient *db.DB,
	gcsClient *storage.Client,
	bigQueryClient *bigquery.Client,
	pinnedDateTime *time.Time,
	cacheClient cache.Cache,
) *Server {

	server := &Server{
		mode:                 mode,
		listenAddr:           listenAddr,
		dashboardCoordinates: dashboardCoordinates,

		syntheticTestManager: syntheticTestManager,
		variantManager:       variantManager,
		testReportGeneratorConfig: TestReportGeneratorConfig{
			TestGridLoadingConfig:       testGridLoadingConfig,
			RawJobResultsAnalysisConfig: rawJobResultsAnalysisOptions,
			DisplayDataConfig:           displayDataOptions,
		},
		sippyNG:        sippyNG,
		static:         static,
		db:             dbClient,
		bigQueryClient: bigQueryClient,
		pinnedDateTime: pinnedDateTime,
		gcsClient:      gcsClient,
		cache:          cacheClient,
	}

	// Fill cache
	if bigQueryClient != nil {
		go api.GetComponentTestVariantsFromBigQuery(bigQueryClient)
	}

	return server
}

var matViewRefreshMetric = promauto.NewHistogramVec(prometheus.HistogramOpts{
	Name:    "sippy_matview_refresh_millis",
	Help:    "Milliseconds to refresh our postgresql materialized views",
	Buckets: []float64{10, 100, 200, 500, 1000, 5000, 10000, 30000, 60000, 300000},
}, []string{"view"})

var allMatViewsRefreshMetric = promauto.NewHistogram(prometheus.HistogramOpts{
	Name:    "sippy_all_matviews_refresh_millis",
	Help:    "Milliseconds to refresh our postgresql materialized views",
	Buckets: []float64{5000, 10000, 30000, 60000, 300000, 600000, 1200000, 1800000, 2400000, 3000000, 3600000},
})

type Server struct {
	mode                 Mode
	listenAddr           string
	dashboardCoordinates []TestGridDashboardCoordinates

	syntheticTestManager       synthetictests.SyntheticTestManager
	variantManager             testidentification.VariantManager
	testReportGeneratorConfig  TestReportGeneratorConfig
	perfscaleMetricsJobReports []workloadmetricsv1.WorkloadMetricsRow
	sippyNG                    fs.FS
	static                     fs.FS
	httpServer                 *http.Server
	db                         *db.DB
	bigQueryClient             *bigquery.Client
	pinnedDateTime             *time.Time
	gcsClient                  *storage.Client
	cache                      cache.Cache
}

type TestGridDashboardCoordinates struct {
	// this is how we index and display.  it gets wired to ?release for now
	ReportName string
	// this is generic and is required
	TestGridDashboardNames []string
	// this is openshift specific, used for BZ lookup and not required
	BugzillaRelease string
}

func (s *Server) GetReportEnd() time.Time {
	return util.GetReportEnd(s.pinnedDateTime)
}

// refreshMaterializedViews updates the postgresql materialized views backing our reports. It is called by the handler
// for the /refresh API endpoint, which is called by the sidecar script which loads the new data from testgrid into the
// main postgresql tables.
//
// refreshMatviewOnlyIfEmpty is used on startup to indicate that we want to do an initial refresh *only* if
// the views appear to be empty.
func refreshMaterializedViews(dbc *db.DB, refreshMatviewOnlyIfEmpty bool) {
	var promPusher *push.Pusher
	if pushgateway := os.Getenv("SIPPY_PROMETHEUS_PUSHGATEWAY"); pushgateway != "" {
		promPusher = push.New(pushgateway, "sippy-matviews")
		promPusher.Collector(matViewRefreshMetric)
		promPusher.Collector(allMatViewsRefreshMetric)
	}

	log.Info("refreshing materialized views")
	allStart := time.Now()

	if dbc == nil {
		log.Info("skipping materialized view refresh as server has no db connection provided")
		return
	}
	// create a channel for work "tasks"
	ch := make(chan string)

	wg := sync.WaitGroup{}

	// allow concurrent workers for refreshing matviews in parallel
	for t := 0; t < 3; t++ {
		wg.Add(1)
		go refreshMatview(dbc, refreshMatviewOnlyIfEmpty, ch, &wg)
	}

	for _, pmv := range db.PostgresMatViews {
		ch <- pmv.Name
	}

	close(ch)
	wg.Wait()

	allElapsed := time.Since(allStart)
	log.WithField("elapsed", allElapsed).Info("refreshed all materialized views")
	allMatViewsRefreshMetric.Observe(float64(allElapsed.Milliseconds()))

	if promPusher != nil {
		log.Info("pushing metrics to prometheus gateway")
		if err := promPusher.Add(); err != nil {
			log.WithError(err).Error("could not push to prometheus pushgateway")
		} else {
			log.Info("successfully pushed metrics to prometheus gateway")
		}
	}
}

func refreshMatview(dbc *db.DB, refreshMatviewOnlyIfEmpty bool, ch chan string, wg *sync.WaitGroup) {

	for matView := range ch {
		start := time.Now()
		tmpLog := log.WithField("matview", matView)

		// If requested, we only refresh the materialized view if it has no rows
		if refreshMatviewOnlyIfEmpty {
			var count int
			if res := dbc.DB.Raw(fmt.Sprintf("SELECT COUNT(*) FROM %s", matView)).Scan(&count); res.Error != nil {
				tmpLog.WithError(res.Error).Warn("proceeding with refresh of matview that appears to be empty")
			} else if count > 0 {
				tmpLog.Info("skipping matview refresh as it appears to be populated")
				continue
			}
		}

		// Try to refresh concurrently, if we get an error that likely means the view has never been
		// populated (could be a developer env, or a schema migration on the view), fall back to the normal
		// refresh which locks reads.
		tmpLog.Info("refreshing materialized view")
		if res := dbc.DB.Exec(
			fmt.Sprintf("REFRESH MATERIALIZED VIEW CONCURRENTLY %s", matView)); res.Error != nil {
			tmpLog.WithError(res.Error).Warn("error refreshing materialized view concurrently, falling back to regular refresh")

			if res := dbc.DB.Exec(
				fmt.Sprintf("REFRESH MATERIALIZED VIEW %s", matView)); res.Error != nil {
				tmpLog.WithError(res.Error).Error("error refreshing materialized view")
			} else {
				elapsed := time.Since(start)
				tmpLog.WithField("elapsed", elapsed).Info("refreshed materialized view")
				matViewRefreshMetric.WithLabelValues(matView).Observe(float64(elapsed.Milliseconds()))
			}

		} else {
			elapsed := time.Since(start)
			tmpLog.WithField("elapsed", elapsed).Info("refreshed materialized view concurrently")
			matViewRefreshMetric.WithLabelValues(matView).Observe(float64(elapsed.Milliseconds()))
		}
	}
	wg.Done()
}

func RefreshData(dbc *db.DB, pinnedDateTime *time.Time, refreshMatviewsOnlyIfEmpty bool) {
	log.Infof("Refreshing data")

	refreshMaterializedViews(dbc, refreshMatviewsOnlyIfEmpty)

	log.Infof("Refresh complete")
}

func (s *Server) reportNameToDashboardCoordinates(reportName string) (TestGridDashboardCoordinates, bool) {
	for _, dashboard := range s.dashboardCoordinates {
		if dashboard.ReportName == reportName {
			return dashboard, true
		}
	}
	return TestGridDashboardCoordinates{}, false
}

func (s *Server) reportNames() []string {
	ret := []string{}
	for _, dashboard := range s.dashboardCoordinates {
		ret = append(ret, dashboard.ReportName)
	}
	return ret
}

func (s *Server) jsonCapabilitiesReport(w http.ResponseWriter, _ *http.Request) {
	capabilities := make([]string, 0)
	if s.mode == ModeOpenShift {
		capabilities = append(capabilities, "openshift_releases")
	}

	if hasBuildCluster, err := query.HasBuildClusterData(s.db); hasBuildCluster {
		capabilities = append(capabilities, "build_clusters")
	} else if err != nil {
		log.WithError(err).Warningf("could not fetch build cluster data")
	}

	api.RespondWithJSON(http.StatusOK, w, capabilities)
}

func (s *Server) jsonAutocompleteFromDB(w http.ResponseWriter, req *http.Request) {
	api.PrintAutocompleteFromDB(w, req, s.db)
}

func (s *Server) jsonReleaseTagsReport(w http.ResponseWriter, req *http.Request) {
	api.PrintReleasesReport(w, req, s.db)
}

func (s *Server) jsonIncidentEvent(w http.ResponseWriter, req *http.Request) {
	start, err := getISO8601Date("start", req)
	if err != nil {
		api.RespondWithJSON(http.StatusInternalServerError, w, map[string]interface{}{"code": http.StatusInternalServerError,
			"message": "couldn't parse start param" + err.Error()})
		return
	}

	end, err := getISO8601Date("end", req)
	if err != nil {
		api.RespondWithJSON(http.StatusInternalServerError, w, map[string]interface{}{"code": http.StatusInternalServerError,
			"message": "couldn't parse start param" + err.Error()})
		return
	}

	results, err := api.GetJIRAIncidentsFromDB(s.db, start, end)
	if err != nil {
		api.RespondWithJSON(http.StatusInternalServerError, w, map[string]interface{}{"code": http.StatusInternalServerError,
			"message": "couldn't fetch events" + err.Error()})
		return
	}

	api.RespondWithJSON(http.StatusOK, w, results)
}

func (s *Server) jsonReleaseTagsEvent(w http.ResponseWriter, req *http.Request) {
	release := s.getReleaseOrFail(w, req)
	if release != "" {
		filterOpts, err := filter.FilterOptionsFromRequest(req, "release_time", apitype.SortDescending)
		if err != nil {
			api.RespondWithJSON(http.StatusInternalServerError, w, map[string]interface{}{"code": http.StatusInternalServerError,
				"message": "couldn't parse filter opts " + err.Error()})
			return
		}

		start, err := getISO8601Date("start", req)
		if err != nil {
			api.RespondWithJSON(http.StatusInternalServerError, w, map[string]interface{}{"code": http.StatusInternalServerError,
				"message": "couldn't parse start param" + err.Error()})
			return
		}

		end, err := getISO8601Date("end", req)
		if err != nil {
			api.RespondWithJSON(http.StatusInternalServerError, w, map[string]interface{}{"code": http.StatusInternalServerError,
				"message": "couldn't parse start param" + err.Error()})
			return
		}

		results, err := api.GetPayloadEvents(s.db, release, filterOpts, start, end)
		if err != nil {
			api.RespondWithJSON(http.StatusInternalServerError, w, map[string]interface{}{"code": http.StatusInternalServerError,
				"message": "couldn't parse start param" + err.Error()})
			return
		}

		api.RespondWithJSON(http.StatusOK, w, results)
	}
}

func (s *Server) jsonReleasePullRequestsReport(w http.ResponseWriter, req *http.Request) {
	api.PrintPullRequestsReport(w, req, s.db)
}

func (s *Server) jsonListPayloadJobRuns(w http.ResponseWriter, req *http.Request) {
	// Release appears optional here, perhaps when listing all job runs for all payloads
	// in the release, but this may not make sense. Likely this API call should be
	// moved away from filters and possible support for multiple payloads at once to
	// URL encoded single payload.
	release := req.URL.Query().Get("release")
	filterOpts, err := filter.FilterOptionsFromRequest(req, "id", apitype.SortDescending)
	if err != nil {
		log.WithError(err).Error("error")
		api.RespondWithJSON(http.StatusInternalServerError, w, map[string]interface{}{"code": http.StatusInternalServerError,
			"message": "Error building job run report:" + err.Error()})
		return
	}

	payloadJobRuns, err := api.ListPayloadJobRuns(s.db, filterOpts, release)
	if err != nil {
		log.WithError(err).Error("error listing payload job runs")
		api.RespondWithJSON(http.StatusBadRequest, w, map[string]interface{}{
			"code":    http.StatusBadRequest,
			"message": err.Error(),
		})
	}
	api.RespondWithJSON(http.StatusOK, w, payloadJobRuns)
}

// TODO: may want to merge with jsonReleaseHealthReport, but this is a fair bit slower, and release health is run
// on startup many times over when we calculate the metrics.
// if we could boil the go logic for building this down into a query, it could become another matview and then
// could be run quickly, assembling into the health api much more easily
func (s *Server) jsonGetPayloadAnalysis(w http.ResponseWriter, req *http.Request) {
	release := req.URL.Query().Get("release")
	if release == "" {
		api.RespondWithJSON(http.StatusBadRequest, w, map[string]interface{}{
			"code":    http.StatusBadRequest,
			"message": fmt.Errorf(`"release" is required`),
		})
		return
	}
	stream := req.URL.Query().Get("stream")
	if release == "" {
		api.RespondWithJSON(http.StatusBadRequest, w, map[string]interface{}{
			"code":    http.StatusBadRequest,
			"message": fmt.Errorf(`"stream" is required`),
		})
		return
	}
	arch := req.URL.Query().Get("arch")
	if release == "" {
		api.RespondWithJSON(http.StatusBadRequest, w, map[string]interface{}{
			"code":    http.StatusBadRequest,
			"message": fmt.Errorf(`"arch" is required`),
		})
		return
	}

	filterOpts, err := filter.FilterOptionsFromRequest(req, "id", apitype.SortDescending)
	if err != nil {
		api.RespondWithJSON(http.StatusInternalServerError, w, map[string]interface{}{"code": http.StatusInternalServerError, "message": err.Error()})
		return
	}

	log.WithFields(log.Fields{
		"release": release,
		"stream":  stream,
		"arch":    arch,
	}).Info("analyzing payload stream")

	result, err := api.GetPayloadStreamTestFailures(s.db, release, stream, arch, filterOpts, s.GetReportEnd())
	if err != nil {
		log.WithError(err).Error("error")
		api.RespondWithJSON(http.StatusInternalServerError, w, map[string]interface{}{"code": http.StatusInternalServerError,
			"message": "Error analyzing payload: " + err.Error()})
		return
	}

	api.RespondWithJSON(http.StatusOK, w, result)
}

// jsonGetPayloadTestFailures is an api to fetch information about what tests failed across all jobs in a specific
// payload.
func (s *Server) jsonGetPayloadTestFailures(w http.ResponseWriter, req *http.Request) {
	payload := req.URL.Query().Get("payload")
	if payload == "" {
		api.RespondWithJSON(http.StatusBadRequest, w, map[string]interface{}{
			"code":    http.StatusBadRequest,
			"message": fmt.Errorf(`"payload" is required`),
		})
		return
	}

	logger := log.WithFields(log.Fields{
		"payload": payload,
	})
	logger.Info("checking for test failures in payload")

	result, err := api.GetPayloadTestFailures(s.db, payload, logger)
	if err != nil {
		log.WithError(err).Error("error")
		api.RespondWithJSON(http.StatusInternalServerError, w, map[string]interface{}{"code": http.StatusInternalServerError,
			"message": "Error looking up test failures for payload: " + err.Error()})
		return
	}

	api.RespondWithJSON(http.StatusOK, w, result)
}

func (s *Server) jsonReleaseHealthReport(w http.ResponseWriter, req *http.Request) {
	release := req.URL.Query().Get("release")
	if release == "" {
		api.RespondWithJSON(http.StatusBadRequest, w, map[string]interface{}{
			"code":    http.StatusBadRequest,
			"message": fmt.Errorf(`"release" is required`),
		})
		return
	}

	results, err := api.ReleaseHealthReports(s.db, release, s.GetReportEnd())
	if err != nil {
		log.WithError(err).Error("error generating release health report")
		api.RespondWithJSON(http.StatusInternalServerError, w, map[string]interface{}{
			"code":    http.StatusInternalServerError,
			"message": err.Error(),
		})
		return
	}

	api.RespondWithJSON(http.StatusOK, w, results)
}

func (s *Server) jsonTestAnalysis(w http.ResponseWriter, req *http.Request, dbFN func(*db.DB, *filter.Filter, string, string, time.Time) (map[string][]api.CountByDate, error)) {
	testName := req.URL.Query().Get("test")
	if testName == "" {
		api.RespondWithJSON(http.StatusBadRequest, w, map[string]interface{}{
			"code":    http.StatusBadRequest,
			"message": "'test' is required.",
		})
		return
	}
	release := s.getReleaseOrFail(w, req)
	if release != "" {
		filters, err := filter.ExtractFilters(req)
		if err != nil {
			api.RespondWithJSON(http.StatusInternalServerError, w, map[string]interface{}{"code": http.StatusInternalServerError,
				"message": "couldn't parse filter opts " + err.Error()})
			return
		}
		results, err := dbFN(s.db, filters, release, testName, s.GetReportEnd())
		if err != nil {
			api.RespondWithJSON(http.StatusInternalServerError, w, map[string]interface{}{"code": http.StatusInternalServerError,
				"message": err.Error()})
			return
		}
		api.RespondWithJSON(200, w, results)
	}
}

func (s *Server) jsonTestAnalysisByJobFromDB(w http.ResponseWriter, req *http.Request) {
	s.jsonTestAnalysis(w, req, api.GetTestAnalysisByJobFromDB)
}

func (s *Server) jsonTestAnalysisByVariantFromDB(w http.ResponseWriter, req *http.Request) {
	s.jsonTestAnalysis(w, req, api.GetTestAnalysisByVariantFromDB)
}

func (s *Server) jsonTestAnalysisOverallFromDB(w http.ResponseWriter, req *http.Request) {
	s.jsonTestAnalysis(w, req, api.GetTestAnalysisOverallFromDB)
}

func (s *Server) jsonTestBugsFromDB(w http.ResponseWriter, req *http.Request) {
	testName := req.URL.Query().Get("test")
	if testName == "" {
		api.RespondWithJSON(http.StatusBadRequest, w, map[string]interface{}{
			"code":    http.StatusBadRequest,
			"message": "'test' is required.",
		})
		return
	}

	bugs, err := query.LoadBugsForTest(s.db, testName, false)
	if err != nil {
		log.WithError(err).Error("error querying test bugs from db")
		api.RespondWithJSON(http.StatusInternalServerError, w, map[string]interface{}{
			"code":    http.StatusInternalServerError,
			"message": "error querying test bugs from db",
		})
		return
	}
	api.RespondWithJSON(http.StatusOK, w, bugs)
}

func (s *Server) jsonTestDurationsFromDB(w http.ResponseWriter, req *http.Request) {
	release := s.getReleaseOrFail(w, req)
	if release == "" {
		return
	}

	testName := req.URL.Query().Get("test")
	if testName == "" {
		api.RespondWithJSON(http.StatusBadRequest, w, map[string]interface{}{
			"code":    http.StatusBadRequest,
			"message": "'test' is required.",
		})
		return
	}

	filters, err := filter.ExtractFilters(req)
	if err != nil {
		api.RespondWithJSON(http.StatusInternalServerError, w, map[string]interface{}{
			"code":    http.StatusInternalServerError,
			"message": "error processing filter options",
		})
		return
	}

	outputs, err := api.GetTestDurationsFromDB(s.db, release, testName, filters)
	if err != nil {
		log.WithError(err).Error("error querying test outputs from db")
		api.RespondWithJSON(http.StatusInternalServerError, w, map[string]interface{}{
			"code":    http.StatusInternalServerError,
			"message": "error querying test outputs from db",
		})
		return
	}
	api.RespondWithJSON(http.StatusOK, w, outputs)
}

func (s *Server) jsonTestOutputsFromDB(w http.ResponseWriter, req *http.Request) {
	release := s.getReleaseOrFail(w, req)
	if release == "" {
		return
	}

	testName := req.URL.Query().Get("test")
	if testName == "" {
		api.RespondWithJSON(http.StatusBadRequest, w, map[string]interface{}{
			"code":    http.StatusBadRequest,
			"message": "'test' is required.",
		})
		return
	}

	filters, err := filter.ExtractFilters(req)
	if err != nil {
		api.RespondWithJSON(http.StatusInternalServerError, w, map[string]interface{}{
			"code":    http.StatusInternalServerError,
			"message": "error processing filter options",
		})
		return
	}

	outputs, err := api.GetTestOutputsFromDB(s.db, release, testName, filters, 10)
	if err != nil {
		log.WithError(err).Error("error querying test outputs from db")
		api.RespondWithJSON(http.StatusInternalServerError, w, map[string]interface{}{
			"code":    http.StatusInternalServerError,
			"message": "error querying test outputs from db",
		})
		return
	}
	api.RespondWithJSON(http.StatusOK, w, outputs)
}

func (s *Server) jsonComponentTestVariantsFromBigQuery(w http.ResponseWriter, req *http.Request) {
	if s.bigQueryClient == nil {
		api.RespondWithJSON(http.StatusBadRequest, w, map[string]interface{}{
			"code":    http.StatusBadRequest,
			"message": "component report API is only available when google-service-account-credential-file is configured",
		})
		return
	}
	outputs, errs := api.GetComponentTestVariantsFromBigQuery(s.bigQueryClient)
	if len(errs) > 0 {
		log.Warningf("%d errors were encountered while querying test variants from big query:", len(errs))
		for _, err := range errs {
			log.Error(err.Error())
		}
		api.RespondWithJSON(http.StatusInternalServerError, w, map[string]interface{}{
			"code":    http.StatusInternalServerError,
			"message": "error querying test variants from big query",
		})
		return
	}
	api.RespondWithJSON(http.StatusOK, w, outputs)
}

func (s *Server) jsonComponentReportFromBigQuery(w http.ResponseWriter, req *http.Request) {
	baseRelease, sampleRelease, testIDOption, variantOption, excludeOption, advancedOption, err := s.parseComponentReportRequest(req)
	if err != nil {
		api.RespondWithJSON(http.StatusBadRequest, w, map[string]interface{}{
			"code":    http.StatusBadRequest,
			"message": err.Error(),
		})
		return
	}

	outputs, errs := api.GetComponentReportFromBigQuery(
		s.bigQueryClient,
		baseRelease,
		sampleRelease,
		testIDOption,
		variantOption,
		excludeOption,
		advancedOption)
	if len(errs) > 0 {
		log.Warningf("%d errors were encountered while querying component from big query:", len(errs))
		for _, err := range errs {
			log.Error(err.Error())
		}
		api.RespondWithJSON(http.StatusInternalServerError, w, map[string]interface{}{
			"code":    http.StatusInternalServerError,
			"message": fmt.Sprintf("error querying component from big query: %v", errs),
		})
		return
	}
	api.RespondWithJSON(http.StatusOK, w, outputs)
}

func (s *Server) jsonComponentReportTestDetailsFromBigQuery(w http.ResponseWriter, req *http.Request) {
	baseRelease, sampleRelease, testIDOption, variantOption, excludeOption, advancedOption, err := s.parseComponentReportRequest(req)
	if err != nil {
		api.RespondWithJSON(http.StatusBadRequest, w, map[string]interface{}{
			"code":    http.StatusBadRequest,
			"message": err.Error(),
		})
		return
	}
	outputs, errs := api.GetComponentReportTestDetailsFromBigQuery(
		s.bigQueryClient,
		baseRelease,
		sampleRelease,
		testIDOption,
		variantOption,
		excludeOption,
		advancedOption)
	if len(errs) > 0 {
		log.Warningf("%d errors were encountered while querying component test details from big query:", len(errs))
		for _, err := range errs {
			log.Error(err.Error())
		}
		api.RespondWithJSON(http.StatusInternalServerError, w, map[string]interface{}{
			"code":    http.StatusInternalServerError,
			"message": fmt.Sprintf("error querying component test details from big query: %v", errs),
		})
		return
	}
	api.RespondWithJSON(http.StatusOK, w, outputs)
}

func (s *Server) parseComponentReportRequest(req *http.Request) (
	apitype.ComponentReportRequestReleaseOptions,
	apitype.ComponentReportRequestReleaseOptions,
	apitype.ComponentReportRequestTestIdentificationOptions,
	apitype.ComponentReportRequestVariantOptions,
	apitype.ComponentReportRequestExcludeOptions,
	apitype.ComponentReportRequestAdvancedOptions,
	error) {
	var baseRelease, sampleRelease apitype.ComponentReportRequestReleaseOptions
	var testIDOption apitype.ComponentReportRequestTestIdentificationOptions
	var variantOption apitype.ComponentReportRequestVariantOptions
	var excludeOption apitype.ComponentReportRequestExcludeOptions
	var advancedOption apitype.ComponentReportRequestAdvancedOptions

	var err error
	if s.bigQueryClient == nil {
		return baseRelease, sampleRelease, testIDOption, variantOption, excludeOption, advancedOption,
			fmt.Errorf("component report API is only available when google-service-account-credential-file is configured")
	}
	baseRelease.Release = req.URL.Query().Get("baseRelease")
	sampleRelease.Release = req.URL.Query().Get("sampleRelease")
	if baseRelease.Release == "" {
		return baseRelease, sampleRelease, testIDOption, variantOption, excludeOption, advancedOption, fmt.Errorf("missing base_release")
	}

	if sampleRelease.Release == "" {
		return baseRelease, sampleRelease, testIDOption, variantOption, excludeOption, advancedOption, fmt.Errorf("missing sample_release")
	}

	timeStr := req.URL.Query().Get("baseStartTime")
	baseRelease.Start, err = time.Parse(time.RFC3339, timeStr)
	if err != nil {
		return baseRelease, sampleRelease, testIDOption, variantOption, excludeOption, advancedOption, fmt.Errorf("base start time in wrong format")
	}
	timeStr = req.URL.Query().Get("baseEndTime")
	baseRelease.End, err = time.Parse(time.RFC3339, timeStr)
	if err != nil {
		return baseRelease, sampleRelease, testIDOption, variantOption, excludeOption, advancedOption, fmt.Errorf("base end time in wrong format")
	}
	timeStr = req.URL.Query().Get("sampleStartTime")
	sampleRelease.Start, err = time.Parse(time.RFC3339, timeStr)
	if err != nil {
		return baseRelease, sampleRelease, testIDOption, variantOption, excludeOption, advancedOption, fmt.Errorf("sample start time in wrong format")
	}
	timeStr = req.URL.Query().Get("sampleEndTime")
	sampleRelease.End, err = time.Parse(time.RFC3339, timeStr)
	if err != nil {
		return baseRelease, sampleRelease, testIDOption, variantOption, excludeOption, advancedOption, fmt.Errorf("sample end time in wrong format")
	}

	testIDOption.Component = req.URL.Query().Get("component")
	testIDOption.Capability = req.URL.Query().Get("capability")
	testIDOption.TestID = req.URL.Query().Get("testId")

	variantOption.GroupBy = req.URL.Query().Get("groupBy")
	variantOption.Platform = req.URL.Query().Get("platform")
	variantOption.Upgrade = req.URL.Query().Get("upgrade")
	variantOption.Arch = req.URL.Query().Get("arch")
	variantOption.Network = req.URL.Query().Get("network")
	variantOption.Variant = req.URL.Query().Get("variant")

	excludeOption.ExcludePlatforms = req.URL.Query().Get("excludeClouds")
	excludeOption.ExcludeArches = req.URL.Query().Get("excludeArches")
	excludeOption.ExcludeNetworks = req.URL.Query().Get("excludeNetworks")
	excludeOption.ExcludeUpgrades = req.URL.Query().Get("excludeUpgrades")
	excludeOption.ExcludeVariants = req.URL.Query().Get("excludeVariants")

	advancedOption.Confidence = 95
	confidenceStr := req.URL.Query().Get("confidence")
	if confidenceStr != "" {
		advancedOption.Confidence, err = strconv.Atoi(confidenceStr)
		if err != nil {
			return baseRelease, sampleRelease, testIDOption, variantOption, excludeOption, advancedOption, fmt.Errorf("confidence is not a number")
		}
		if advancedOption.Confidence < 0 || advancedOption.Confidence > 100 {
			return baseRelease, sampleRelease, testIDOption, variantOption, excludeOption, advancedOption, fmt.Errorf("confidence is not in the correct range")
		}
	}

	advancedOption.PityFactor = 5
	pityStr := req.URL.Query().Get("pity")
	if pityStr != "" {
		advancedOption.PityFactor, err = strconv.Atoi(pityStr)
		if err != nil {
			return baseRelease, sampleRelease, testIDOption, variantOption, excludeOption, advancedOption, fmt.Errorf("pity factor is not a number")
		}
		if advancedOption.PityFactor < 0 || advancedOption.PityFactor > 100 {
			return baseRelease, sampleRelease, testIDOption, variantOption, excludeOption, advancedOption, fmt.Errorf("pity factor is not in the correct range")
		}
	}

	advancedOption.MinimumFailure = 3
	minFailStr := req.URL.Query().Get("minFail")
	if minFailStr != "" {
		advancedOption.MinimumFailure, err = strconv.Atoi(minFailStr)
		if err != nil {
			return baseRelease, sampleRelease, testIDOption, variantOption, excludeOption, advancedOption, fmt.Errorf("min_fail is not a number")
		}
		if advancedOption.MinimumFailure < 0 {
			return baseRelease, sampleRelease, testIDOption, variantOption, excludeOption, advancedOption, fmt.Errorf("min_fail is not in the correct range")
		}
	}

	advancedOption.IgnoreMissing = false
	ignoreMissingStr := req.URL.Query().Get("ignoreMissing")
	if ignoreMissingStr != "" {
		if ignoreMissingStr == "true" {
			advancedOption.IgnoreMissing = true
		} else if ignoreMissingStr != "false" {
			return baseRelease, sampleRelease, testIDOption, variantOption, excludeOption, advancedOption, fmt.Errorf("missing parameter is in the wrong format")
		}

	}

	advancedOption.IgnoreDisruption = true
	ignoreDisruptionsStr := req.URL.Query().Get("ignoreDisruption")
	if ignoreDisruptionsStr != "" {
		if ignoreDisruptionsStr == "false" {
			advancedOption.IgnoreDisruption = false
		} else if ignoreDisruptionsStr != "true" {
			return baseRelease, sampleRelease, testIDOption, variantOption, excludeOption, advancedOption, fmt.Errorf("ignore disruption is in the wrong format")
		}
	}

	return baseRelease, sampleRelease, testIDOption, variantOption, excludeOption, advancedOption, err
}

func (s *Server) jsonJobBugsFromDB(w http.ResponseWriter, req *http.Request) {
	release := s.getRelease(req)

	fil, err := filter.ExtractFilters(req)
	if err != nil {
		api.RespondWithJSON(http.StatusBadRequest, w, map[string]interface{}{"code": http.StatusBadRequest, "message": "Could not marshal query:" + err.Error()})
		return
	}
	jobFilter, _, err := splitJobAndJobRunFilters(fil)
	if err != nil {
		api.RespondWithJSON(http.StatusBadRequest, w, map[string]interface{}{"code": http.StatusBadRequest, "message": "Could not marshal query:" + err.Error()})
		return
	}

	start, boundary, end := getPeriodDates("default", req, s.GetReportEnd())
	limit := getLimitParam(req)
	sortField, sort := getSortParams(req)

	jobIDs, err := query.ListFilteredJobIDs(s.db, release, jobFilter, start, boundary, end, limit, sortField, sort)
	if err != nil {
		log.WithError(err).Error("error querying jobs")
		api.RespondWithJSON(http.StatusInternalServerError, w, map[string]interface{}{
			"code":    http.StatusInternalServerError,
			"message": "error querying jobs",
		})
		return
	}

	bugs, err := query.LoadBugsForJobs(s.db, jobIDs, false)
	if err != nil {
		log.WithError(err).Error("error querying job bugs from db")
		api.RespondWithJSON(http.StatusInternalServerError, w, map[string]interface{}{
			"code":    http.StatusInternalServerError,
			"message": "error querying job bugs from db",
		})
		return
	}
	api.RespondWithJSON(http.StatusOK, w, bugs)
}

func (s *Server) jsonTestsReportFromDB(w http.ResponseWriter, req *http.Request) {
	release := s.getReleaseOrFail(w, req)
	if release != "" {
		api.PrintTestsJSONFromDB(release, w, req, s.db)
	}
}

func (s *Server) jsonTestDetailsReportFromDB(w http.ResponseWriter, req *http.Request) {
	// Filter to test names containing this query param:
	testSubstring := req.URL.Query()["test"]
	release := s.getReleaseOrFail(w, req)
	if release != "" {
		api.PrintTestsDetailsJSONFromDB(w, release, testSubstring, s.db)
	}
}

func (s *Server) jsonReleasesReportFromDB(w http.ResponseWriter, _ *http.Request) {
	response := apitype.Releases{
		GADates: releasesync.GADateMap,
	}
	releases, err := query.ReleasesFromDB(s.db)
	if err != nil {
		log.WithError(err).Error("error querying releases from db")
		api.RespondWithJSON(http.StatusInternalServerError, w, map[string]interface{}{
			"code":    http.StatusInternalServerError,
			"message": "error querying releases from db",
		})
		return
	}

	for _, release := range releases {
		response.Releases = append(response.Releases, release.Release)
	}

	type LastUpdated struct {
		Max time.Time
	}
	var lastUpdated LastUpdated
	// Assume our last update is the last time we inserted a prow job run.
	res := s.db.DB.Raw("SELECT MAX(created_at) FROM prow_job_runs").Scan(&lastUpdated)
	if res.Error != nil {
		log.WithError(res.Error).Error("error querying last updated from db")
		api.RespondWithJSON(http.StatusInternalServerError, w, map[string]interface{}{
			"code":    http.StatusInternalServerError,
			"message": "error querying last updated from db",
		})
		return
	}

	response.LastUpdated = lastUpdated.Max
	api.RespondWithJSON(http.StatusOK, w, response)
}

func (s *Server) jsonHealthReportFromDB(w http.ResponseWriter, req *http.Request) {
	release := s.getReleaseOrFail(w, req)
	if release != "" {
		api.PrintOverallReleaseHealthFromDB(w, s.db, release, s.GetReportEnd())
	}
}

func (s *Server) jsonBuildClusterHealth(w http.ResponseWriter, req *http.Request) {
	start, boundary, end := getPeriodDates("default", req, s.GetReportEnd())

	results, err := api.GetBuildClusterHealthReport(s.db, start, boundary, end)
	if err != nil {
		log.WithError(err).Error("error querying build cluster health from db")
		api.RespondWithJSON(http.StatusInternalServerError, w, map[string]interface{}{
			"code":    http.StatusInternalServerError,
			"message": "error querying build cluster health from db " + err.Error(),
		})
		return
	}

	api.RespondWithJSON(200, w, results)
}

func (s *Server) jsonBuildClusterHealthAnalysis(w http.ResponseWriter, req *http.Request) {
	period := req.URL.Query().Get("period")
	if period == "" {
		period = api.PeriodDay
	}

	results, err := api.GetBuildClusterHealthAnalysis(s.db, period)
	if err != nil {
		log.WithError(err).Error("error querying build cluster health from db")
		api.RespondWithJSON(http.StatusInternalServerError, w, map[string]interface{}{
			"code":    http.StatusInternalServerError,
			"message": "error querying build cluster health from db " + err.Error(),
		})
		return
	}

	api.RespondWithJSON(200, w, results)
}

func (s *Server) getRelease(req *http.Request) string {
	return req.URL.Query().Get("release")
}

func (s *Server) getReleaseOrFail(w http.ResponseWriter, req *http.Request) string {
	release := req.URL.Query().Get("release")

	if release == "" {
		api.RespondWithJSON(http.StatusBadRequest, w, map[string]interface{}{
			"code":    "400",
			"message": "release is required",
		})
		return release
	}

	return release
}

func (s *Server) jsonJobsDetailsReportFromDB(w http.ResponseWriter, req *http.Request) {
	release := s.getReleaseOrFail(w, req)
	jobName := req.URL.Query().Get("job")
	if release != "" && jobName != "" {
		err := api.PrintJobDetailsReportFromDB(w, req, s.db, release, jobName, s.GetReportEnd())
		if err != nil {
			log.Errorf("Error from PrintJobDetailsReportFromDB: %v", err)
		}
	}
}

func (s *Server) printReportDate(w http.ResponseWriter, req *http.Request) {
	reportDate := ""
	if s.pinnedDateTime != nil {
		reportDate = s.pinnedDateTime.Format(time.RFC3339)
	}
	api.RespondWithJSON(http.StatusOK, w, map[string]interface{}{"pinnedDateTime": reportDate})
}

func (s *Server) printCanaryReportFromDB(w http.ResponseWriter, req *http.Request) {
	release := s.getReleaseOrFail(w, req)
	if release != "" {
		api.PrintCanaryTestsFromDB(release, w, s.db)
	}
}

func (s *Server) jsonVariantsReportFromDB(w http.ResponseWriter, req *http.Request) {
	release := s.getReleaseOrFail(w, req)
	if release != "" {
		api.PrintVariantReportFromDB(w, req, s.db, release, s.GetReportEnd())
	}
}

func (s *Server) jsonJobsReportFromDB(w http.ResponseWriter, req *http.Request) {
	release := s.getReleaseOrFail(w, req)
	if release != "" {
		api.PrintJobsReportFromDB(w, req, s.db, release, s.GetReportEnd())
	}
}

func (s *Server) jsonRepositoriesReportFromDB(w http.ResponseWriter, req *http.Request) {
	release := s.getReleaseOrFail(w, req)
	if release != "" {
		filterOpts, err := filter.FilterOptionsFromRequest(req, "premerge_job_failures", apitype.SortDescending)
		if err != nil {
			api.RespondWithJSON(http.StatusInternalServerError, w, map[string]interface{}{"code": http.StatusInternalServerError,
				"message": "couldn't parse filter opts " + err.Error()})
			return
		}

		results, err := api.GetRepositoriesReportFromDB(s.db, release, filterOpts, s.GetReportEnd())
		if err != nil {
			log.WithError(err).Error("error")
			api.RespondWithJSON(http.StatusInternalServerError, w, map[string]interface{}{"code": http.StatusInternalServerError,
				"message": "Error fetching repositories " + err.Error()})
			return
		}

		api.RespondWithJSON(http.StatusOK, w, results)
	}
}

func (s *Server) jsonPullRequestsReportFromDB(w http.ResponseWriter, req *http.Request) {
	release := s.getReleaseOrFail(w, req)
	if release != "" {
		filterOpts, err := filter.FilterOptionsFromRequest(req, "merged_at", apitype.SortDescending)
		if err != nil {
			api.RespondWithJSON(http.StatusInternalServerError, w, map[string]interface{}{"code": http.StatusInternalServerError,
				"message": "couldn't parse filter opts " + err.Error()})
			return
		}

		results, err := api.GetPullRequestsReportFromDB(s.db, release, filterOpts)
		if err != nil {
			log.WithError(err).Error("error")
			api.RespondWithJSON(http.StatusInternalServerError, w, map[string]interface{}{"code": http.StatusInternalServerError,
				"message": "Error fetching pull requests" + err.Error()})
			return
		}

		api.RespondWithJSON(http.StatusOK, w, results)
	}
}

func (s *Server) jsonJobRunsReportFromDB(w http.ResponseWriter, req *http.Request) {
	release := s.getRelease(req)

	filterOpts, err := filter.FilterOptionsFromRequest(req, "timestamp", "desc")
	if err != nil {
		api.RespondWithJSON(http.StatusBadRequest, w, map[string]interface{}{"code": http.StatusBadRequest, "message": "Could not marshal query:" + err.Error()})
		return
	}

	pagination, err := getPaginationParams(req)
	if err != nil {
		api.RespondWithJSON(http.StatusBadRequest, w, map[string]interface{}{"code": http.StatusBadRequest, "message": "Could not parse pagination options: " + err.Error()})
		return
	}

	result, err := api.JobsRunsReportFromDB(s.db, filterOpts, release, pagination, s.GetReportEnd())
	if err != nil {
		api.RespondWithJSON(http.StatusBadRequest, w, map[string]interface{}{"code": http.StatusBadRequest, "message": err.Error()})
		return
	}

	api.RespondWithJSON(http.StatusOK, w, result)
}

// jsonJobRunRiskAnalysis is an API to make a guess at the severity of failures in a prow job run, based on historical
// pass rates for each failed test, on-going incidents, and other factors.
//
// This API can be called in two ways, a GET with a prow_job_run_id query param, or a GET with a
// partial ProwJobRun struct serialized as json in the request body. The ID version will return the
// stored analysis for the job when it was imported into sippy. The other version is a transient
// request to be used when sippy has not yet imported the job, but we wish to analyze the failure risk.
// Soon, we expect the transient version is called from CI to get a risk analysis json result, which will
// be stored in the job run artifacts, then imported with the job run, and will ultimately be the
// data that is returned by the get by ID version.
func (s *Server) jsonJobRunRiskAnalysis(w http.ResponseWriter, req *http.Request) {

	logger := log.WithField("func", "jsonJobRunRiskAnalysis")

	jobRun := &models.ProwJobRun{}
	var jobRunTestCount int

	// API path one where we return a risk analysis for a prow job run ID we already know about:
	jobRunIDStr := req.URL.Query().Get("prow_job_run_id")
	if jobRunIDStr != "" {

		jobRunID, err := strconv.ParseInt(jobRunIDStr, 10, 64)
		if err != nil {
			api.RespondWithJSON(http.StatusBadRequest, w, map[string]interface{}{
				"code":    http.StatusBadRequest,
				"message": "unable to parse prow_job_run_id: " + err.Error()})
			return
		}

		logger = logger.WithField("jobRunID", jobRunID)

		// lookup prowjob and run count
		jobRun, jobRunTestCount, err = api.FetchJobRun(s.db, jobRunID, logger)

		if err != nil {
			api.RespondWithJSON(http.StatusBadRequest, w, map[string]interface{}{
				"code": http.StatusBadRequest, "message": err.Error()})
			return
		}

	} else {
		err := json.NewDecoder(req.Body).Decode(&jobRun)
		if err != nil {
			api.RespondWithJSON(http.StatusBadRequest, w, map[string]interface{}{
				"code":    http.StatusBadRequest,
				"message": fmt.Sprintf("error decoding prow job run json in request body: %s", err)})
			return
		}

		// validate the jobRun isn't empty
		// valid case where test artifacts are not available
		// we want to mark this as a high risk
		if isValid, detailReason := isValidProwJobRun(jobRun); !isValid {

			log.Warn("Invalid ProwJob provided for analysis, returning elevated risk")
			result := apitype.ProwJobRunRiskAnalysis{
				OverallRisk: apitype.FailureRisk{
					Level:   apitype.FailureRiskLevelMissingData,
					Reasons: []string{fmt.Sprintf("Invalid ProwJob provided for analysis: %s", detailReason)},
				},
			}

			// respond ok since we handle it
			api.RespondWithJSON(http.StatusOK, w, result)
			return
		}

		jobRunTestCount = jobRun.TestCount

		// We don't expect the caller to fully populate the ProwJob, just it's name,
		// override the input by looking up the actual ProwJob so we have access to release and variants.
		job := &models.ProwJob{}
		res := s.db.DB.Where("name = ?", jobRun.ProwJob.Name).First(job)
		if res.Error != nil {
			api.RespondWithJSON(http.StatusBadRequest, w, map[string]interface{}{
				"code":    http.StatusBadRequest,
				"message": fmt.Sprintf("unable to find ProwJob: %s", jobRun.ProwJob.Name)})
			return
		}
		jobRun.ProwJob = *job

		// if the ClusterData is being passed in then use it to override the variants (agnostic case, etc)
		if jobRun.ClusterData.Release != "" {
			jobRun.ProwJob.Variants = s.variantManager.IdentifyVariants(jobRun.ProwJob.Name, jobRun.ClusterData.Release, jobRun.ClusterData)
		}
		logger = logger.WithField("jobRunID", jobRun.ID)
	}

	logger.Infof("job run = %+v", *jobRun)
	result, err := api.JobRunRiskAnalysis(s.db, jobRun, jobRunTestCount, logger.WithField("func", "JobRunRiskAnalysis"))
	if err != nil {
		api.RespondWithJSON(http.StatusBadRequest, w, map[string]interface{}{
			"code":    http.StatusBadRequest,
			"message": err.Error()})
		return
	}

	api.RespondWithJSON(http.StatusOK, w, result)
}

// jsonJobRunRiskAnalysis is an API to return the intervals origin builds for interesting things that occurred during
// the test run.
//
// This API is used by the job run intervals chart in the UI.
func (s *Server) jsonJobRunIntervals(w http.ResponseWriter, req *http.Request) {

	logger := log.WithField("func", "jsonJobRunIntervals")

	if s.gcsClient == nil {
		api.RespondWithJSON(http.StatusBadRequest, w, map[string]interface{}{
			"code":    http.StatusBadRequest,
			"message": "server not configured for GCS, unable to use this API"})
		return
	}

	jobRunIDStr := req.URL.Query().Get("prow_job_run_id")
	if jobRunIDStr == "" {
		api.RespondWithJSON(http.StatusBadRequest, w, map[string]interface{}{
			"code": http.StatusBadRequest, "message": "prow_job_run_id query parameter not specified"})
		return
	}

	jobRunID, err := strconv.ParseInt(jobRunIDStr, 10, 64)
	if err != nil {
		api.RespondWithJSON(http.StatusBadRequest, w, map[string]interface{}{
			"code":    http.StatusBadRequest,
			"message": "unable to parse prow_job_run_id: " + err.Error()})
		return
	}
	logger = logger.WithField("jobRunID", jobRunID)

	result, err := jobrunintervals.JobRunIntervals(s.gcsClient, s.db, jobRunID, logger.WithField("func", "JobRunRiskIntervals"))
	if err != nil {
		api.RespondWithJSON(http.StatusBadRequest, w, map[string]interface{}{
			"code":    http.StatusBadRequest,
			"message": err.Error()})
		return
	}

	api.RespondWithJSON(http.StatusOK, w, result)
}

func isValidProwJobRun(jobRun *models.ProwJobRun) (bool, string) {
	if (jobRun == nil || jobRun == &models.ProwJobRun{} || &jobRun.ProwJob == &models.ProwJob{} || jobRun.ProwJob.Name == "") {

		detailReason := "empty ProwJobRun"

		if (jobRun != nil && jobRun != &models.ProwJobRun{}) {

			// not likely to be empty when we have a non empty ProwJobRun
			detailReason = "empty ProwJob"

			if (&jobRun.ProwJob != &models.ProwJob{}) {
				detailReason = "missing ProwJob Name"
			}
		}

		return false, detailReason
	}

	return true, ""
}

func (s *Server) jsonJobsAnalysisFromDB(w http.ResponseWriter, req *http.Request) {
	release := s.getRelease(req)

	fil, err := filter.ExtractFilters(req)
	if err != nil {
		api.RespondWithJSON(http.StatusBadRequest, w, map[string]interface{}{"code": http.StatusBadRequest, "message": "Could not marshal query:" + err.Error()})
		return
	}
	jobFilter, jobRunsFilter, err := splitJobAndJobRunFilters(fil)
	if err != nil {
		api.RespondWithJSON(http.StatusBadRequest, w, map[string]interface{}{"code": http.StatusBadRequest, "message": "Could not marshal query:" + err.Error()})
		return
	}

	start, boundary, end := getPeriodDates("default", req, s.GetReportEnd())
	limit := getLimitParam(req)
	sortField, sort := getSortParams(req)

	period := req.URL.Query().Get("period")
	if period == "" {
		period = api.PeriodDay
	}

	results, err := api.PrintJobAnalysisJSONFromDB(s.db, release, jobFilter, jobRunsFilter,
		start, boundary, end, limit, sortField, sort, period, s.GetReportEnd())
	if err != nil {
		log.WithError(err).Error("error in PrintJobAnalysisJSONFromDB")
		api.RespondWithJSON(http.StatusInternalServerError, w, map[string]interface{}{"code": http.StatusInternalServerError, "message": err.Error()})
		return
	}

	api.RespondWithJSON(http.StatusOK, w, results)
}

func (s *Server) jsonPerfScaleMetricsReport(w http.ResponseWriter, req *http.Request) {
	reports := s.perfscaleMetricsJobReports

	release := s.getReleaseOrFail(w, req)
	if release != "" {
		api.PrintPerfscaleWorkloadMetricsReport(w, req, release, reports)
	}
}

func (s *Server) Serve() {
	// Use private ServeMux to prevent tests from stomping on http.DefaultServeMux
	serveMux := http.NewServeMux()

	// Handle serving React version of frontend with support for browser router, i.e. anything not found
	// goes to index.html
	serveMux.HandleFunc("/sippy-ng/", func(w http.ResponseWriter, r *http.Request) {
		fs := s.sippyNG
		if r.URL.Path != "/sippy-ng/" {
			fullPath := strings.TrimPrefix(r.URL.Path, "/sippy-ng/")
			if _, err := fs.Open(fullPath); err != nil {
				if !os.IsNotExist(err) {
					w.WriteHeader(http.StatusNotFound)
					w.Header().Set("Content-Type", "text/plain")
					if _, err := w.Write([]byte(fmt.Sprintf("404 Not Found: %s", fullPath))); err != nil {
						log.WithError(err).Warningf("could not write response")
					}
					return
				}
				r.URL.Path = "/sippy-ng/"
			}
		}
		http.StripPrefix("/sippy-ng/", http.FileServer(http.FS(fs))).ServeHTTP(w, r)
	})

	serveMux.Handle("/static/", http.FileServer(http.FS(s.static)))

	// Re-direct "/" to sippy-ng
	serveMux.HandleFunc("/", func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/" {
			http.NotFound(w, req)
			return
		}
		http.Redirect(w, req, "/sippy-ng/", 301)
	})

	serveMux.HandleFunc("/api/autocomplete/", s.jsonAutocompleteFromDB)
	serveMux.HandleFunc("/api/jobs", s.jsonJobsReportFromDB)
	serveMux.HandleFunc("/api/jobs/runs", s.jsonJobRunsReportFromDB)
	serveMux.HandleFunc("/api/jobs/runs/risk_analysis", s.jsonJobRunRiskAnalysis)
	serveMux.HandleFunc("/api/jobs/runs/intervals", s.cached(4*time.Hour, s.jsonJobRunIntervals))
	serveMux.HandleFunc("/api/jobs/analysis", s.jsonJobsAnalysisFromDB)
	serveMux.HandleFunc("/api/jobs/details", s.jsonJobsDetailsReportFromDB)
	serveMux.HandleFunc("/api/jobs/bugs", s.jsonJobBugsFromDB)
	serveMux.HandleFunc("/api/pull_requests", s.cached(1*time.Hour, s.jsonPullRequestsReportFromDB))
	serveMux.HandleFunc("/api/repositories", s.jsonRepositoriesReportFromDB)
	serveMux.HandleFunc("/api/tests", s.jsonTestsReportFromDB)
	serveMux.HandleFunc("/api/tests/details", s.cached(1*time.Hour, s.jsonTestDetailsReportFromDB))
	serveMux.HandleFunc("/api/tests/analysis/overall", s.cached(1*time.Hour, s.jsonTestAnalysisOverallFromDB))
	serveMux.HandleFunc("/api/tests/analysis/variants", s.cached(1*time.Hour, s.jsonTestAnalysisByVariantFromDB))
	serveMux.HandleFunc("/api/tests/analysis/jobs", s.cached(1*time.Hour, s.jsonTestAnalysisByJobFromDB))
	serveMux.HandleFunc("/api/tests/bugs", s.jsonTestBugsFromDB)
	serveMux.HandleFunc("/api/tests/outputs", s.cached(1*time.Hour, s.jsonTestOutputsFromDB))
	serveMux.HandleFunc("/api/tests/durations", s.cached(1*time.Hour, s.jsonTestDurationsFromDB))
	serveMux.HandleFunc("/api/install", s.cached(1*time.Hour, s.jsonInstallReportFromDB))
	serveMux.HandleFunc("/api/upgrade", s.cached(1*time.Hour, s.jsonUpgradeReportFromDB))
	serveMux.HandleFunc("/api/releases", s.jsonReleasesReportFromDB)
	serveMux.HandleFunc("/api/health/build_cluster/analysis", s.jsonBuildClusterHealthAnalysis)
	serveMux.HandleFunc("/api/health/build_cluster", s.jsonBuildClusterHealth)
	serveMux.HandleFunc("/api/health", s.jsonHealthReportFromDB)
	serveMux.HandleFunc("/api/variants", s.jsonVariantsReportFromDB)
	serveMux.HandleFunc("/api/canary", s.printCanaryReportFromDB)
	serveMux.HandleFunc("/api/report_date", s.printReportDate)
	serveMux.HandleFunc("/api/component_readiness", s.cached(4*time.Hour, s.jsonComponentReportFromBigQuery))
	serveMux.HandleFunc("/api/component_readiness/variants", s.cached(4*time.Hour, s.jsonComponentTestVariantsFromBigQuery))
	serveMux.HandleFunc("/api/component_readiness/test_details", s.cached(4*time.Hour, s.jsonComponentReportTestDetailsFromBigQuery))

	serveMux.HandleFunc("/api/perfscalemetrics", s.jsonPerfScaleMetricsReport)
	serveMux.HandleFunc("/api/capabilities", s.jsonCapabilitiesReport)
	if s.db != nil {
		serveMux.HandleFunc("/api/releases/health", s.jsonReleaseHealthReport)
		serveMux.HandleFunc("/api/releases/tags/events", s.jsonReleaseTagsEvent)
		serveMux.HandleFunc("/api/releases/tags", s.jsonReleaseTagsReport)
		serveMux.HandleFunc("/api/releases/pull_requests", s.jsonReleasePullRequestsReport)
		serveMux.HandleFunc("/api/releases/job_runs", s.jsonListPayloadJobRuns)
		serveMux.HandleFunc("/api/incidents", s.jsonIncidentEvent)

		serveMux.HandleFunc("/api/releases/test_failures",
			s.jsonGetPayloadAnalysis)

		serveMux.HandleFunc("/api/payloads/test_failures",
			s.jsonGetPayloadTestFailures)
	}

	var handler http.Handler = serveMux
	// wrap mux with our logger. this will
	handler = logRequestHandler(handler)
	// ... potentially add more middleware handlers

	// Store a pointer to the HTTP server for later retrieval.
	s.httpServer = &http.Server{
		Addr:              s.listenAddr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	log.Infof("Serving reports on %s ", s.listenAddr)

	if err := s.httpServer.ListenAndServe(); err != http.ErrServerClosed {
		log.WithError(err).Error("Server exited")
	}
}

func logRequestHandler(h http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		h.ServeHTTP(w, r)
		log.WithFields(log.Fields{
			"uri":     r.URL.String(),
			"method":  r.Method,
			"elapsed": time.Since(start),
		}).Info("responded to request")
	}
	return http.HandlerFunc(fn)
}

func (s *Server) cached(duration time.Duration, handler func(w http.ResponseWriter, r *http.Request)) func(http.ResponseWriter, *http.Request) {
	if s.cache == nil {
		log.Debugf("no cache configured, making live api call")
		return handler
	}

	return func(w http.ResponseWriter, r *http.Request) {
		content, err := s.cache.Get(r.RequestURI)
		if err != nil { // cache miss
			log.WithError(err).Debugf("cache miss: could not fetch data from cache for %q", r.RequestURI)
		} else if content != nil && respondFromCache(content, w, r) == nil { // cache hit
			return
		}
		recordResponse(s.cache, duration, w, r, handler)
	}
}

func respondFromCache(content []byte, w http.ResponseWriter, r *http.Request) error {
	apiResponse := cache.APIResponse{}
	if err := json.Unmarshal(content, &apiResponse); err != nil {
		log.WithError(err).Warningf("couldn't unmarshal api response")
		return err
	}
	log.Debugf("cache hit for %q", r.RequestURI)
	for k, v := range apiResponse.Headers {
		w.Header()[k] = v
	}
	w.Header().Set("X-Sippy-Cached", "true")
	w.WriteHeader(http.StatusOK)

	if _, err := w.Write(apiResponse.Response); err != nil {
		log.WithError(err).Debugf("error writing http response")
		return err
	}

	return nil
}

func recordResponse(c cache.Cache, duration time.Duration, w http.ResponseWriter, r *http.Request, handler func(w http.ResponseWriter, r *http.Request)) {
	apiResponse := cache.APIResponse{}
	recorder := httptest.NewRecorder()
	handler(recorder, r)
	apiResponse.Headers = w.Header()
	for k, v := range recorder.Result().Header {
		w.Header()[k] = v
	}
	w.WriteHeader(recorder.Code)
	content := recorder.Body.Bytes()
	apiResponse.Response = content

	log.Debugf("caching new page: %s for %s\n", r.RequestURI, duration)
	apiResponseBytes, err := json.Marshal(apiResponse)
	if err != nil {
		log.WithError(err).Warningf("couldn't marshal api response")
	}

	if err := c.Set(r.RequestURI, apiResponseBytes, duration); err != nil {
		log.WithError(err).Warningf("could not cache page")
	}
	if _, err := w.Write(content); err != nil {
		log.WithError(err).Debugf("error writing http response")
	}
}

func (s *Server) GetHTTPServer() *http.Server {
	return s.httpServer
}
