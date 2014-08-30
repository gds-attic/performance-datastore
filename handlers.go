package main

import (
	"encoding/json"
	"fmt"
	"github.com/go-martini/martini"
	"github.com/jabley/performance-datastore/config_api"
	"labix.org/v2/mgo"
	"labix.org/v2/mgo/bson"
	"net/http"
	"sync"
	"time"
)

type statusResponse struct {
	// Field names should be public, so that encoding/json can see them
	Status  string `json:"status"`
	Message string `json:"message"`
	Code    int    `json:"code"`
}

type WarningResponse struct {
	Data    interface{}
	Warning string `json:"warning"`
}

type Query struct {
}

var (
	mgoSession      *mgo.Session
	mgoDatabaseName = "backdrop"
	mgoURL          = "localhost"
)

type DataSetMetaData map[string]interface{}

type DataSet struct {
	storage  *mgo.Session
	metaData DataSetMetaData
}

func getMgoSession() *mgo.Session {
	if mgoSession == nil {
		var err error
		mgoSession, err = mgo.DialWithTimeout(mgoURL, 5*time.Second)
		if err != nil {
			panic(err)
		}
		// Set timeout to suitably small value by default.
		mgoSession.SetSyncTimeout(5 * time.Second)
	}
	return mgoSession.Copy()
}

// statusHandler is the basic healthcheck for the application
func statusHandler(w http.ResponseWriter, r *http.Request) {
	session := getMgoSession()
	defer session.Close()

	session.SetMode(mgo.Eventual, true)

	status := statusResponse{
		Status:  "ok",
		Message: "database seems fine",
	}

	if addrs := session.LiveServers(); len(addrs) == 0 {
		status.Status = "error"
		status.Message = "cannot connect to database"
		w.WriteHeader(http.StatusInternalServerError)
	}

	setStatusHeaders(w)
	serialiseJSON(w, status)
}

type dataSetStatusResponse struct {
	// Field names should be public, so that encoding/json can see them
	Status   string `json:"status"`
	Message  string `json:"message"`
	Code     int    `json:"code"`
	DataSets []DataSetStatus
}

type DataSetStatus struct {
	Name             string    `json:"name"`
	SecondsOutOfDate int       `json:"seconds-out-of-date"`
	LastUpdated      time.Time `json:"last-updated"`
	MaxAgeExpected   int       `json:"max-age-expected"`
}

// dataSetStatusHandler is basic healthcheck for all of the datasets
func dataSetStatusHandler(w http.ResponseWriter, r *http.Request) {
	session := getMgoSession()
	defer session.Close()

	session.SetMode(mgo.Eventual, true)

	datasets, err := config_api.ListDataSets()

	if err != nil {
		panic(err)
	}

	failing := collectStaleness(datasets, session)
	status := summariseStaleness(failing)

	setStatusHeaders(w)
	serialiseJSON(w, status)
}

// GET|OPTIONS /:data_set_name
func dataSetHandler(w http.ResponseWriter, r *http.Request, params martini.Params) {
	metaData, err := config_api.DataSet(params["data_set_name"])
	if err != nil {
		panic(err)
	}
	fetch(metaData, w, r)
}

// GET|OPTIONS /data/:data_group/data_type
func dataTypeHandler(w http.ResponseWriter, r *http.Request, params martini.Params) {
	metaData, err := config_api.DataType(params["data_group"], params["data_type"])
	if err != nil {
		panic(err)
	}
	fetch(metaData, w, r)
}

func logAndReturn(w http.ResponseWriter, message string) {
	w.WriteHeader(http.StatusNotFound)
	setStatusHeaders(w)
	serialiseJSON(w, statusResponse{"error", message, 0})
}

func fetch(metaData DataSetMetaData, w http.ResponseWriter, r *http.Request) {
	if metaData == nil {
		logAndReturn(w, "data_set not found")
		return
	}

	session := getMgoSession()
	defer session.Close()

	session.SetMode(mgo.Eventual, true)

	dataSet := DataSet{session, metaData}

	// Is the data set queryable?
	if !dataSet.isQueryable() {
		logAndReturn(w, fmt.Sprintf("data_set %s not found", dataSet.Name()))
		return
	}

	// OPTIONS?
	if r.Method == "OPTIONS" {
		// TODO Set allowed methods
		w.Header().Set("Access-Control-Max-Age", "86400")
		w.Header().Set("Access-Control-Allow-Headers", "Cache-Control")
		return
	}

	if validationResult := validateRequest(r, dataSet); validationResult.invalid {
		logAndReturn(w, fmt.Sprintf(validationResult.message, dataSet.Name()))
		return
	}

	query := parseQuery(r)
	data, err := dataSet.Execute(query)

	if err != nil {
		logAndReturn(w, fmt.Sprintf("Invalid collect function", dataSet.Name()))
		return
	}

	var body interface{}

	if !dataSet.isPublished() {
		warning := "Warning: This data-set is unpublished. \n" +
			"Data may be subject to change or be inaccurate."
		w.Header().Set("Cache-Control", "no-cache")
		body = WarningResponse{data, warning}
	} else {
		maxAge := dataSet.CacheDuration()
		w.Header().Set("Cache-Control", fmt.Sprintf("max-age=%d, must-revalidate", maxAge))
		body = data
	}

	serialiseJSON(w, body)
}

func parseQuery(r *http.Request) Query {
	return Query{}
}

func validateRequest(r *http.Request, dataSet DataSet) (v ValidationResult) {
	return
}

type ValidationResult struct {
	invalid bool
	message string
}

func serialiseJSON(w http.ResponseWriter, status interface{}) {
	encoder := json.NewEncoder(w)
	if err := encoder.Encode(status); err != nil {
		panic(err)
	}
}

func checkFreshness(
	dataSet DataSet,
	failing chan DataSetStatus,
	wg *sync.WaitGroup) {
	defer wg.Done()

	if dataSet.isStale() && dataSet.isPublished() {
		failing <- DataSetStatus{dataSet.Name(), 0, time.Now(), 0}
	}
}

func setStatusHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "none")
}

func collectStaleness(datasets []interface{}, session *mgo.Session) (failing chan DataSetStatus) {
	wg := &sync.WaitGroup{}
	wg.Add(len(datasets))
	failing = make(chan DataSetStatus, len(datasets))

	for _, dataset := range datasets {
		go checkFreshness(DataSet{session, dataset.(DataSetMetaData)}, failing, wg)
	}

	wg.Wait()

	return
}

func (d DataSet) isQueryable() bool {
	return d.booleanValue("queryable")
}

func (d DataSet) isStale() bool {
	expectedMaxAge := d.getMaxExpectedAge()
	now := time.Now()
	lastUpdated := d.getLastUpdated()

	if isStalenessAppropriate(expectedMaxAge, lastUpdated) {
		return now.Sub(*lastUpdated) > time.Duration(*expectedMaxAge)
	}

	return false
}

func (d DataSet) getMaxExpectedAge() (maxExpectedAge *int64) {
	value, ok := d.metaData["max_age_expected"].(int64)

	// where does the responsibility for setting a default lie? I suggest
	// within the Configuration API
	if ok {
		maxExpectedAge = &value
	}

	return
}

func (d DataSet) Execute(query Query) (interface{}, error) {
	return nil, nil
}

func (d DataSet) isPublished() bool {
	return d.booleanValue("published")
}

func (d DataSet) isRealtime() bool {
	return d.booleanValue("realtime")
}

func (d DataSet) CacheDuration() int {
	if d.isRealtime() {
		return 120
	}
	return 1800
}

func (d DataSet) booleanValue(field string) (result bool) {
	value, ok := d.metaData[field].(bool)

	if ok {
		result = value
	}

	return
}

func (d DataSet) Name() string {
	return d.metaData["name"].(string)
}

func (d DataSet) getLastUpdated() (t *time.Time) {
	var lastUpdated bson.M
	d.storage.SetMode(mgo.Monotonic, true)

	coll := d.storage.DB("backdrop").C(d.Name())
	err := coll.Find(nil).Sort("-_updated_at").One(&lastUpdated)

	if err != nil {
		panic(err)
	}

	t = nil

	value, isTime := lastUpdated["_updated_at"].(time.Time)

	if isTime {
		t = &value
	}
	return
}

// isStalenessAppropriate returns false if there is no limit on
// expected max age or the data set has never been updated, otherwise
// returns true
func isStalenessAppropriate(maxAge *int64, lastUpdated *time.Time) bool {
	return maxAge != nil && lastUpdated != nil
}

func summariseStaleness(failing chan DataSetStatus) dataSetStatusResponse {
	allGood := true

	message := "All data-sets are in date"

	var failures []DataSetStatus

	for failure := range failing {
		allGood = false
		failures = append(failures, failure)
	}

	if allGood {
		return dataSetStatusResponse{
			Status:  "ok",
			Message: message,
		}
	} else {
		message = fmt.Sprintf("%d %s out of date", len(failures), pluraliseDataSets(failures))
		return dataSetStatusResponse{
			Status:   "not okay",
			Message:  message,
			DataSets: failures,
		}
	}
}

func pluraliseDataSets(failures []DataSetStatus) string {
	if len(failures) > 1 {
		return "data-sets are"
	} else {
		return "data-set is"
	}
}
