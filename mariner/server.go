package mariner

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/gorilla/mux"

	"github.com/uc-cdis/go-authutils/authutils"
)

// this file contains code for setting up the mariner-server
// and registering handler functions for the various api endpoints
// NOTE: server is modeled after arborist

// WorkflowRequest holds the unmarshalled body of the POST request
// where
// Workflow is the packed CWL workflow JSON (i.e., all the CWL files packed into a JSON - ii.ee., the result of cwltool --pack)
// Input is the JSON specifying values for the input parameters to the workflow (refer to files using GUIDs)
// ID is the userID
// HERE - TODO - eventually replace "ID" field with "token"
// ---> then need to retrieve user ID by trade token with Fence
type WorkflowRequest struct {
	Workflow json.RawMessage `json:"workflow"`
	Input    json.RawMessage `json:"input"`
	UserID   string          `json:"user"`
	Manifest Manifest        `json:"manifest"`
}

// HERE - TODO - move to config.go
type Manifest []ManifestEntry
type ManifestEntry struct {
	GUID string `json:"object_id"`
}

type JWTDecoder interface {
	Decode(string) (*map[string]interface{}, error) // not sure why this is a pointer to a map? map is already passed by reference
}

type Server struct {
	jwtApp JWTDecoder
	logger *LogHandler
}

// move to log.go
// see Arborist's logging.go
// need to integrate or ow handle server logging vs. workflow logging
type LogHandler struct {
	logger *log.Logger
}

func (server *Server) withJWTApp(jwtApp JWTDecoder) *Server {
	server.jwtApp = jwtApp
	return server
}

// TODO - see logging in mariner - implement server logging for mariner
func (server *Server) withLogger(logger *log.Logger) *Server {
	server.logger = &LogHandler{logger: logger}
	return server
}

func server() (server *Server) {
	return &Server{}
}

// first just getting the endpoints to work, then will make nice and WES-ish
func (server *Server) makeRouter(out io.Writer) http.Handler {
	router := mux.NewRouter().StrictSlash(true)
	router.HandleFunc("/runs", server.handleRunsPOST).Methods("POST")                     // OKAY
	router.HandleFunc("/runs", server.handleRunsGET).Methods("GET")                       // OKAY
	router.HandleFunc("/runs/{runID}", server.handleRunLogGET).Methods("GET")             // OKAY
	router.HandleFunc("/runs/{runID}/cancel", server.handleCancelRunPOST).Methods("POST") // TODO
	router.HandleFunc("/runs/{runID}/status", server.handleRunStatusGET).Methods("GET")   // OKAY
	router.HandleFunc("/_status", server.handleHealthCheck).Methods("GET")                // TO CHECK

	router.Use(server.handleAuth)        // use auth middleware function - right now access to mariner API is all-or-nothing
	router.Use(server.setResponseHeader) // set "Content-Type: application/json" header - every endpoint returns JSON
	return router
}

// a run's unique key is the pair (userID, runID)
func (server *Server) uniqueKey(r *http.Request) (userID, runID string) {
	runID = mux.Vars(r)["runID"]
	userID = server.userID(r)
	return userID, runID
}

type RunLogJSON struct {
	Log *MainLog `json:"log"`
}

func (j *RunLogJSON) fetchLog(userID, runID string) error {
	runLog, err := fetchMainLog(userID, runID)
	if err != nil {
		return err
	}
	j.Log = runLog
	return nil
}

// '/runs/{runID}' - GET
func (server *Server) handleRunLogGET(w http.ResponseWriter, r *http.Request) {
	userID, runID := server.uniqueKey(r)
	j := (&RunLogJSON{}).fetchLog(userID, runID)
	json.NewEncoder(w).Encode(j)
}

// '/runs/{runID}/status' - GET
func (server *Server) handleRunStatusGET(w http.ResponseWriter, r *http.Request) {
	userID, runID := server.uniqueKey(r)
	j := (&StatusJSON{}).fetchStatus(userID, runID)
	json.NewEncoder(w).Encode(j)
}

func (j *StatusJSON) fetchStatus(userID, runID string) error {
	runLog, err := fetchMainLog(userID, runID)
	if err != nil {
		return err
	}
	j.Status = runLog.Main.Status
	return nil
}

type StatusJSON struct {
	Status string `json:"status"`
}

// '/runs/{runID}/cancel' - POST
func (server *Server) handleCancelRunPOST(w http.ResponseWriter, r *http.Request) {

}

type ListRunsJSON struct {
	RunIDs []string `json:"runIDs"`
}

// '/runs' - GET
func (server *Server) handleRunsGET(w http.ResponseWriter, r *http.Request) {
	userID := server.userID(r)
	j := (&ListRunsJSON{}).fetchRuns(userID)
	json.NewEncoder(w).Encode(j)
}

func (j *ListRunsJSON) fetchRuns(userID string) error {
	runIDs, err := listRuns(userID)
	if err != nil {
		return err
	}
	j.RunIDs = runIDs
	return nil
}

// `/runs` - POST - OKAY
// handles a POST request to run a workflow by dispatching the workflow job
// see "../testdata/request_body.json" for an example of a valid request body
// also see above description of the fields of the WorkflowRequest struct
// since those are the same fields as the request body
// NOTE: come up with uniform, sensible names for handler functions
func (server *Server) handleRunsPOST(w http.ResponseWriter, r *http.Request) {
	workflowRequest := unmarshalBody(r, &WorkflowRequest{}).(*WorkflowRequest)
	workflowRequest.UserID = server.userID(r)
	runID, err := dispatchWorkflowJob(workflowRequest)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	j := &RunIDJSON{RunID: runID}
	json.NewEncoder(w).Encode(j)
}

type RunIDJSON struct {
	RunID string `json:"runID"`
}

// RunServer runs the mariner server that listens for API calls
func RunServer() {
	jwkEndpointEnv := os.Getenv("JWKS_ENDPOINT") // TODO - this is in cloud-automation branch feat/mariner_jwks - need to test, then merge to master

	// Parse flags:
	//     - port (to serve on)
	//     - jwks (endpoint to get keys for JWT validation)
	port := flag.Uint("port", 80, "port on which to expose the API")
	jwkEndpoint := flag.String(
		"jwks",
		jwkEndpointEnv,
		"endpoint from which the application can fetch a JWKS",
	)
	logFlags := log.Ldate | log.Ltime
	logger := log.New(os.Stdout, "", logFlags)
	jwtApp := authutils.NewJWTApplication(*jwkEndpoint)
	server := server().withLogger(logger).withJWTApp(jwtApp)
	router := server.makeRouter(os.Stdout)
	addr := fmt.Sprintf(":%d", *port)
	httpLogger := log.New(os.Stdout, "", log.LstdFlags)
	httpServer := &http.Server{
		Addr:         addr,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		ErrorLog:     httpLogger,
		Handler:      router,
	}
	httpLogger.Println(fmt.Sprintf("mariner serving at %s", httpServer.Addr))
	httpLogger.Fatal(httpServer.ListenAndServe())
}

// unmarshal the request body to the given go struct
func unmarshalBody(r *http.Request, v interface{}) interface{} {
	b := body(r)
	err := json.Unmarshal(b, v)
	if err != nil {
		fmt.Println("error unmarshalling: ", err)
	}
	return v
}

func body(r *http.Request) []byte {
	b, err := ioutil.ReadAll(r.Body)
	if err != nil {
		fmt.Println("error reading body: ", err)
	}
	r.Body.Close()
	r.Body = ioutil.NopCloser(bytes.NewBuffer(b))
	return b
}

type AuthHTTPRequest struct {
	URL         string
	ContentType string
	Body        io.Reader
}

type RequestJSON struct {
	User    *UserJSON    `json:"user"`
	Request *AuthRequest `json:"request"`
}

type AuthRequest struct {
	Resource string      `json:"resource"`
	Action   *AuthAction `json:"action"`
}

type AuthAction struct {
	Service string `json:"service"`
	Method  string `json:"method"`
}

type UserJSON struct {
	Token string `json:"token"`
}

// auth middleware - processes every request, checks auth with arborist
// if arborist says 'okay', then process the request
// if arborist says 'not okay', then http error 'not authorized'
// need to have router.Use(authRequest) or something like that - need to add it to router
func (server *Server) handleAuth(next http.Handler) http.Handler {
	fmt.Println("in auth middleware function..")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Println("checking auth..")
		if server.authZ(r) {
			fmt.Println("user has access")
			next.ServeHTTP(w, r)
			return
		}
		fmt.Println("user does NOT have access")
		http.Error(w, "user not authorized to access this resource", 403)
	})
}

// all endpoints return JSON, so just set that response header here
func (server *Server) setResponseHeader(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		next.ServeHTTP(w, r)
	})
}

// polish this
func authHTTPRequest(r *http.Request) (*AuthHTTPRequest, error) {
	token := r.Header.Get(AUTH_HEADER)
	if token == "" {
		return nil, fmt.Errorf("no token in Authorization header")
	}
	user := &UserJSON{
		Token: token,
	}
	// double check these things
	authRequest := &AuthRequest{
		Resource: "/mariner",
	}
	authAction := &AuthAction{
		Service: "mariner",
		Method:  "access",
	}
	authRequest.Action = authAction
	requestJSON := &RequestJSON{
		User:    user,
		Request: authRequest,
	}
	fmt.Println("here is auth request JSON:")
	printJSON(requestJSON)
	b, err := json.Marshal(requestJSON)
	if err != nil {
		fmt.Println("error marhsaling authRequest to json: ", err)
	}
	authHTTPRequest := &AuthHTTPRequest{
		URL:         "http://arborist-service/auth/request",
		ContentType: "application/json",
		Body:        bytes.NewBuffer(b),
	}
	return authHTTPRequest, nil
}

func (server *Server) authZ(r *http.Request) bool {
	authHTTPRequest, err := authHTTPRequest(r)
	if err != nil {
		fmt.Println("error building auth request: ", err)
		return false
	}
	resp, err := http.Post(
		authHTTPRequest.URL,
		authHTTPRequest.ContentType,
		authHTTPRequest.Body,
	)
	if err != nil {
		// insert better error and logging handling here
		fmt.Println("error asking arborist: ", err)
		return false
	}
	authResponse := &ArboristResponse{}
	b, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		fmt.Println("error reading arborist response body: ", err)
		return false
	}
	resp.Body.Close()
	err = json.Unmarshal(b, authResponse)
	if err != nil {
		fmt.Println("error unmarshalling arborist response to struct: ", err)
		return false
	}
	fmt.Println("here is the arborist response:")
	printJSON(authResponse)
	return authResponse.Auth
}

type ArboristResponse struct {
	Auth bool `json:"auth"`
}

// HandleHealthcheck registers root endpoint
func (server *Server) handleHealthCheck(w http.ResponseWriter, r *http.Request) {
	fmt.Println(r.URL)
	return
}
