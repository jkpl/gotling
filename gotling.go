/**
The MIT License (MIT)

Copyright (c) 2015 ErikL
Copyright (c) 2017 Jaakko Pallari

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
*/
package main

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"fmt"
	"github.com/NodePrime/jsonpath"
	"gopkg.in/xmlpath.v2"
	"gopkg.in/yaml.v2"
	"io/ioutil"
	"log"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

var SimulationStart time.Time

var data []map[string]string
var index = 0

var l sync.Mutex

// "public" synchronized channel for delivering feeder data
var FeedChannel chan map[string]string

var w *bufio.Writer
var f *os.File
var err error

var opened = false

var conn net.Conn

var udpconn *net.UDPConn

var re = regexp.MustCompile(`\$\{([a-zA-Z0-9]{0,})\}`)

func buildActionList(t *TestDef) ([]Action, bool) {
	valid := true
	actions := make([]Action, len(t.Actions))
	for _, element := range t.Actions {
		for key, value := range element {
			var action Action
			actionMap := value.(map[interface{}]interface{})
			switch key {
			case "sleep":
				action = NewSleepAction(actionMap)
			case "http":
				action = NewHttpAction(actionMap)
			case "tcp":
				action = NewTcpAction(actionMap)
			case "udp":
				action = NewUdpAction(actionMap)
			default:
				valid = false
				log.Fatal("Unknown action type encountered: " + key)
			}
			if valid {
				actions = append(actions, action)
			}
		}
	}
	return actions, valid
}

func getBody(action map[interface{}]interface{}) string {
	if action["body"] != nil {
		return action["body"].(string)
	}
	return ""
}

func getTemplate(action map[interface{}]interface{}) string {
	if action["template"] != nil {
		var templateFile = action["template"].(string)
		dir, _ := os.Getwd()
		templateData, _ := ioutil.ReadFile(dir + "/templates/" + templateFile)
		return string(templateData)
	}
	return ""
}

type Action interface {
	Execute(resultsChannel chan HttpReqResult, sessionMap map[string]string)
}

func NextFromFeeder() {

	if len(data) > 0 {

		// Push data into the FeedChannel
		// fmt.Printf("Current index: %d of total size: %d\n", index, len(data))
		FeedChannel <- data[index]

		// Cycle, does this need to be synchronized?
		l.Lock()
		if index < len(data)-1 {
			index++
		} else {
			index = 0
		}
		l.Unlock()
	}

}

func Csv(filename string, separator string) {
	dir, _ := os.Getwd()
	file, _ := os.Open(dir + "/data/" + filename)

	scanner := bufio.NewScanner(file)
	lines := 0

	data = make([]map[string]string, 0)

	// Scan the first line, should contain headers.
	scanner.Scan()
	headers := strings.Split(scanner.Text(), separator)

	for scanner.Scan() {
		line := strings.Split(scanner.Text(), separator)
		item := make(map[string]string)
		for n := 0; n < len(headers); n++ {
			item[headers[n]] = line[n]
		}
		data = append(data, item)
		lines += 1
	}

	if err := scanner.Err(); err != nil {
		log.Fatal(err)
	}
	index = 0
	fmt.Printf("CSV feeder fed with %d lines of data\n", lines)
	FeedChannel = make(chan map[string]string)
}

func main() {

	spec := parseSpecFile()

	SimulationStart = time.Now()
	dir, _ := os.Getwd()
	dat, _ := ioutil.ReadFile(dir + "/" + spec)

	var t TestDef
	err := yaml.Unmarshal(dat, &t)
	if err != nil {
		log.Fatal(err)
	}

	if !ValidateTestDefinition(&t) {
		return
	}

	actions, isValid := buildActionList(&t)
	if !isValid {
		return
	}

	if t.Feeder.Type == "csv" {
		Csv(t.Feeder.Filename, ",")
	} else if t.Feeder.Type != "" {
		log.Fatal("Unsupported feeder type: " + t.Feeder.Type)
	}

	OpenResultsFile(dir + "/results/log/latest.log")
	spawnUsers(&t, actions)

	fmt.Printf("Done in %v\n", time.Since(SimulationStart))
	fmt.Println("Building reports, please wait...")
	CloseResultsFile()
}

func parseSpecFile() string {
	if len(os.Args) == 1 {
		log.Fatal("Cannot start simulation, no YAML simulaton specification supplied as command-line argument")
	}
	var s, sep string
	for i := 1; i < len(os.Args); i++ {
		s += sep + os.Args[i]
		sep = " "
	}
	if s == "" {
		log.Fatalf("Specified simulation file '%s' is not a .yml file", s)
	}
	return s
}

func spawnUsers(t *TestDef, actions []Action) {
	resultsChannel := make(chan HttpReqResult, 10000) // buffer?
	wg := sync.WaitGroup{}
	for i := 0; i < t.Users; i++ {
		wg.Add(1)
		uid := genUserId(t.Users)
		go launchActions(t, resultsChannel, &wg, actions, uid)
		waitDuration := float32(t.Rampup) / float32(t.Users)
		time.Sleep(time.Duration(int(1000*waitDuration)) * time.Millisecond)
	}
	fmt.Println("All users started, waiting at WaitGroup")
	wg.Wait()
}

func genUserId(userCount int) string {
	return strconv.Itoa(rand.Intn(userCount+1) + 10000)
}

func launchActions(t *TestDef, resultsChannel chan HttpReqResult, wg *sync.WaitGroup, actions []Action, uid string) {
	var sessionMap = make(map[string]string)

	for i := 0; i < t.Iterations; i++ {

		// Make sure the sessionMap is cleared before each iteration - except for the UID which stays
		cleanSessionMapAndResetUID(uid, sessionMap)

		// If we have feeder data, pop an item and push its key-value pairs into the sessionMap
		feedSession(t, sessionMap)

		// Iterate over the actions. Note the use of the command-pattern like Execute method on the Action interface
		for _, action := range actions {
			if action != nil {
				action.(Action).Execute(resultsChannel, sessionMap)
			}
		}
	}
	wg.Done()
}

func cleanSessionMapAndResetUID(uid string, sessionMap map[string]string) {
	// Optimization? Delete all entries rather than reallocate map from scratch for each new iteration.
	for k := range sessionMap {
		delete(sessionMap, k)
	}
	sessionMap["UID"] = uid
}

func feedSession(t *TestDef, sessionMap map[string]string) {
	if t.Feeder.Type != "" {
		go NextFromFeeder()       // Do async
		feedData := <-FeedChannel // Will block here until feeder delivers value over the FeedChannel
		for item := range feedData {
			sessionMap[item] = feedData[item]
		}
	}
}

type HttpAction struct {
	Method          string              `yaml:"method"`
	Url             string              `yaml:"url"`
	Body            string              `yaml:"body"`
	Template        string              `yaml:"template"`
	Accept          string              `yaml:"accept"`
	ContentType     string              `yaml:"contentType"`
	Title           string              `yaml:"title"`
	ResponseHandler HttpResponseHandler `yaml:"response"`
	StoreCookie     string              `yaml:"storeCookie"`
}

func (h HttpAction) Execute(resultsChannel chan HttpReqResult, sessionMap map[string]string) {
	DoHttpRequest(h, resultsChannel, sessionMap)
}

type HttpResponseHandler struct {
	Jsonpath string `yaml:"jsonpath"`
	Xmlpath  string `yaml:"xmlpath"`
	Variable string `yaml:"variable"`
	Index    string `yaml:"index"`
}

func NewHttpAction(a map[interface{}]interface{}) HttpAction {
	valid := true
	if a["url"] == "" || a["url"] == nil {
		log.Println("Error: HttpAction must define a URL.")
		valid = false
	}
	if a["method"] == nil || (a["method"] != "GET" && a["method"] != "POST" && a["method"] != "PUT" && a["method"] != "DELETE") {
		log.Println("Error: HttpAction must specify a HTTP method: GET, POST, PUT or DELETE")
		valid = false
	}
	if a["title"] == nil || a["title"] == "" {
		log.Println("Error: HttpAction must define a title.")
		valid = false
	}

	if a["body"] != nil && a["template"] != nil {
		log.Println("Error: A HttpAction can not define both a 'body' and a 'template'.")
		valid = false
	}

	if a["response"] != nil {
		r := a["response"].(map[interface{}]interface{})
		if r["index"] == nil || r["index"] == "" || (r["index"] != "first" && r["index"] != "last" && r["index"] != "random") {
			log.Println("Error: HttpAction ResponseHandler must define an Index of either of: first, last or random.")
			valid = false
		}
		if (r["jsonpath"] == nil || r["jsonpath"] == "") && (r["xmlpath"] == nil || r["xmlpath"] == "") {
			log.Println("Error: HttpAction ResponseHandler must define a Jsonpath or a Xmlpath.")
			valid = false
		}
		if (r["jsonpath"] != nil && r["jsonpath"] != "") && (r["xmlpath"] != nil && r["xmlpath"] != "") {
			log.Println("Error: HttpAction ResponseHandler can only define either a Jsonpath OR a Xmlpath.")
			valid = false
		}

		// TODO perhaps compile Xmlpath expressions so we can validate early?

		if r["variable"] == nil || r["variable"] == "" {
			log.Println("Error: HttpAction ResponseHandler must define a Variable.")
			valid = false
		}
	}

	if !valid {
		log.Fatalf("Your YAML defintion contains an invalid HttpAction, see errors listed above.")
	}
	var responseHandler HttpResponseHandler
	if a["response"] != nil {
		response := a["response"].(map[interface{}]interface{})

		if response["jsonpath"] != nil && response["jsonpath"] != "" {
			responseHandler.Jsonpath = response["jsonpath"].(string)
		}
		if response["xmlpath"] != nil && response["xmlpath"] != "" {
			responseHandler.Xmlpath = response["xmlpath"].(string)
		}

		responseHandler.Variable = response["variable"].(string)
		responseHandler.Index = response["index"].(string)
	}

	accept := "text/html,application/json,application/xhtml+xml,application/xml,text/plain"
	if a["accept"] != nil && len(a["accept"].(string)) > 0 {
		accept = a["accept"].(string)
	}

	var contentType string
	if a["contentType"] != nil && len(a["contentType"].(string)) > 0 {
		contentType = a["contentType"].(string)
	}

	var storeCookie string
	if a["storeCookie"] != nil && a["storeCookie"].(string) != "" {
		storeCookie = a["storeCookie"].(string)
	}

	httpAction := HttpAction{
		a["method"].(string),
		a["url"].(string),
		getBody(a),
		getTemplate(a),
		accept,
		contentType,
		a["title"].(string),
		responseHandler,
		storeCookie,
	}

	return httpAction
}

// Accepts a Httpaction and a one-way channel to write the results to.
func DoHttpRequest(httpAction HttpAction, resultsChannel chan HttpReqResult, sessionMap map[string]string) {
	req := buildHttpRequest(httpAction, sessionMap)

	start := time.Now()
	var DefaultTransport http.RoundTripper = &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	resp, err := DefaultTransport.RoundTrip(req)

	if err != nil {
		log.Printf("HTTP request failed: %s", err)
	} else {
		elapsed := time.Since(start)
		responseBody, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			log.Printf("Reading HTTP response failed: %s\n", err)
			httpReqResult := buildHttpResult(0, resp.StatusCode, elapsed.Nanoseconds(), httpAction.Title)

			resultsChannel <- httpReqResult
		} else {
			defer resp.Body.Close()

			if httpAction.StoreCookie != "" {
				for _, cookie := range resp.Cookies() {

					if cookie.Name == httpAction.StoreCookie {
						sessionMap["____"+cookie.Name] = cookie.Value
					}
				}
			}

			// if action specifies response action, parse using regexp/jsonpath
			processResult(httpAction, sessionMap, responseBody)

			httpReqResult := buildHttpResult(len(responseBody), resp.StatusCode, elapsed.Nanoseconds(), httpAction.Title)

			resultsChannel <- httpReqResult
		}
	}
}

func buildHttpResult(contentLength int, status int, elapsed int64, title string) HttpReqResult {
	httpReqResult := HttpReqResult{
		"HTTP",
		elapsed,
		contentLength,
		status,
		title,
		time.Since(SimulationStart).Nanoseconds(),
	}
	return httpReqResult
}

func buildHttpRequest(httpAction HttpAction, sessionMap map[string]string) *http.Request {
	var req *http.Request
	var err error
	if httpAction.Body != "" {
		reader := strings.NewReader(SubstParams(sessionMap, httpAction.Body))
		req, err = http.NewRequest(httpAction.Method, SubstParams(sessionMap, httpAction.Url), reader)
	} else if httpAction.Template != "" {
		reader := strings.NewReader(SubstParams(sessionMap, httpAction.Template))
		req, err = http.NewRequest(httpAction.Method, SubstParams(sessionMap, httpAction.Url), reader)
	} else {
		req, err = http.NewRequest(httpAction.Method, SubstParams(sessionMap, httpAction.Url), nil)
	}
	if err != nil {
		log.Fatal(err)
	}

	// Add headers
	req.Header.Add("Accept", httpAction.Accept)
	if httpAction.ContentType != "" {
		req.Header.Add("Content-Type", httpAction.ContentType)
	}

	// Add cookies stored by subsequent requests in the sessionMap having the kludgy ____ prefix
	for key, value := range sessionMap {
		if strings.HasPrefix(key, "____") {

			cookie := http.Cookie{
				Name:  key[4:],
				Value: value,
			}

			req.AddCookie(&cookie)
		}
	}

	return req
}

/**
 * If the httpAction specifies a Jsonpath in the Response, try to extract value(s)
 * from the responseBody.
 *
 * TODO extract both Jsonpath handling and Xmlpath handling into separate functions, and write tests for them.
 *
 * Uses github.com/NodePrime/jsonpath
 */
func processResult(httpAction HttpAction, sessionMap map[string]string, responseBody []byte) {
	if httpAction.ResponseHandler.Jsonpath != "" {
		paths, err := jsonpath.ParsePaths(httpAction.ResponseHandler.Jsonpath)
		if err != nil {
			panic(err)
		}
		eval, err := jsonpath.EvalPathsInBytes(responseBody, paths)
		if err != nil {
			panic(err)
		}

		// TODO optimization: Don't reinitialize each time, reuse this somehow.
		resultsArray := make([]string, 0, 10)
		for {
			if result, ok := eval.Next(); ok {

				value := strings.TrimSpace(result.Pretty(false))
				resultsArray = append(resultsArray, trimChar(value, '"'))
			} else {
				break
			}
		}
		if eval.Error != nil {
			panic(eval.Error)
		}

		passResultIntoSessionMap(resultsArray, httpAction, sessionMap)
	}

	if httpAction.ResponseHandler.Xmlpath != "" {
		path := xmlpath.MustCompile(httpAction.ResponseHandler.Xmlpath)
		r := bytes.NewReader(responseBody)
		root, err := xmlpath.Parse(r)

		if err != nil {
			log.Fatal(err)
		}

		iterator := path.Iter(root)
		hasNext := iterator.Next()
		if hasNext {
			resultsArray := make([]string, 0, 10)
			for {
				if hasNext {
					node := iterator.Node()
					resultsArray = append(resultsArray, node.String())
					hasNext = iterator.Next()
				} else {
					break
				}
			}
			passResultIntoSessionMap(resultsArray, httpAction, sessionMap)
		}
	}
}

/**
 * Trims leading and trailing byte r from string s
 */
func trimChar(s string, r byte) string {
	sz := len(s)

	if sz > 0 && s[sz-1] == r {
		s = s[:sz-1]
	}
	sz = len(s)
	if sz > 0 && s[0] == r {
		s = s[1:sz]
	}
	return s
}

func passResultIntoSessionMap(resultsArray []string, httpAction HttpAction, sessionMap map[string]string) {
	resultCount := len(resultsArray)

	if resultCount > 0 {
		switch httpAction.ResponseHandler.Index {
		case FIRST:
			sessionMap[httpAction.ResponseHandler.Variable] = resultsArray[0]
		case LAST:
			sessionMap[httpAction.ResponseHandler.Variable] = resultsArray[resultCount-1]
		case RANDOM:
			if resultCount > 1 {
				sessionMap[httpAction.ResponseHandler.Variable] = resultsArray[rand.Intn(resultCount-1)]
			} else {
				sessionMap[httpAction.ResponseHandler.Variable] = resultsArray[0]
			}
		}

	}
	// TODO how to handle requested, but missing result?
}

type HttpReqResult struct {
	Type    string
	Latency int64
	Size    int
	Status  int
	Title   string
	When    int64
}

func OpenResultsFile(fileName string) {
	if !opened {
		opened = true
	} else {
		return
	}
	f, err = os.Create(fileName)
	if err != nil {
		os.Mkdir("results", 0777)
		os.Mkdir("results/log", 0777)
		f, err = os.Create(fileName)
		if err != nil {
			panic(err)
		}
	}
	w = bufio.NewWriter(f)
	_, err = w.WriteString(string("var logdata = '"))
}

func CloseResultsFile() {
	if opened {
		_, err = w.WriteString(string("';"))
		w.Flush()
		f.Close()
	}
	// Do nothing if not opened
}

type SleepAction struct {
	Duration int `yaml:"duration"`
}

func (s SleepAction) Execute(resultsChannel chan HttpReqResult, sessionMap map[string]string) {
	time.Sleep(time.Duration(s.Duration) * time.Second)
}

func NewSleepAction(a map[interface{}]interface{}) SleepAction {
	return SleepAction{a["duration"].(int)}
}

type TcpAction struct {
	Address string `yaml:"address"`
	Payload string `yaml:"payload"`
	Title   string `yaml:"title"`
}

func (t TcpAction) Execute(resultsChannel chan HttpReqResult, sessionMap map[string]string) {
	DoTcpRequest(t, resultsChannel, sessionMap)
}

func NewTcpAction(a map[interface{}]interface{}) TcpAction {

	// TODO validation
	return TcpAction{
		a["address"].(string),
		a["payload"].(string),
		a["title"].(string),
	}
}

// Accepts a TcpAction and a one-way channel to write the results to.
func DoTcpRequest(tcpAction TcpAction, resultsChannel chan HttpReqResult, sessionMap map[string]string) {

	address := SubstParams(sessionMap, tcpAction.Address)
	payload := SubstParams(sessionMap, tcpAction.Payload)

	if conn == nil {

		conn, err = net.Dial("tcp", address)
		if err != nil {
			fmt.Printf("TCP socket closed, error: %s\n", err)
			conn = nil
			return
		}
	}

	start := time.Now()

	_, err = fmt.Fprintf(conn, payload+"\r\n")
	if err != nil {
		fmt.Printf("TCP request failed with error: %s\n", err)
		conn = nil
	}

	elapsed := time.Since(start)
	resultsChannel <- buildTcpResult(0, 200, elapsed.Nanoseconds(), tcpAction.Title)

}

func buildTcpResult(contentLength int, status int, elapsed int64, title string) HttpReqResult {
	httpReqResult := HttpReqResult{
		"TCP",
		elapsed,
		contentLength,
		status,
		title,
		time.Since(SimulationStart).Nanoseconds(),
	}
	return httpReqResult
}

const FIRST = "first"
const LAST = "last"
const RANDOM = "random"

type TestDef struct {
	Iterations int                      `yaml:"iterations"`
	Users      int                      `yaml:"users"`
	Rampup     int                      `yaml:"rampup"`
	Feeder     Feeder                   `yaml:"feeder"`
	Actions    []map[string]interface{} `yaml:"actions"`
}

type Feeder struct {
	Type     string `yaml:"type"`
	Filename string `yaml:"filename"`
}

// TODO refactor this so it runs before parsing the actions

func ValidateTestDefinition(t *TestDef) bool {
	valid := true
	if t.Iterations == 0 {
		log.Println("Iterations not set, must be > 0")
		valid = false
	}
	if t.Rampup < 0 {
		log.Println("Rampup not defined. must be > -1")
		valid = false
	}
	if t.Users == 0 {
		log.Println("Users must be > 0")
		valid = false
	}
	return valid
}

type UdpAction struct {
	Address string `yaml:"address"`
	Payload string `yaml:"payload"`
	Title   string `yaml:"title"`
}

func (t UdpAction) Execute(resultsChannel chan HttpReqResult, sessionMap map[string]string) {
	DoUdpRequest(t, resultsChannel, sessionMap)
}

func NewUdpAction(a map[interface{}]interface{}) UdpAction {

	// TODO validation
	return UdpAction{
		a["address"].(string),
		a["payload"].(string),
		a["title"].(string),
	}
}

// Accepts a UdpAction and a one-way channel to write the results to.
func DoUdpRequest(udpAction UdpAction, resultsChannel chan HttpReqResult, sessionMap map[string]string) {

	address := SubstParams(sessionMap, udpAction.Address)
	payload := SubstParams(sessionMap, udpAction.Payload)

	if udpconn == nil {
		ServerAddr, err := net.ResolveUDPAddr("udp", address)
		if err != nil {
			fmt.Println("Error ResolveUDPAddr remote: " + err.Error())
		}

		LocalAddr, err := net.ResolveUDPAddr("udp", "127.0.0.1:0")
		if err != nil {
			fmt.Println("Error ResolveUDPAddr local: " + err.Error())
		}

		udpconn, err = net.DialUDP("udp", LocalAddr, ServerAddr)
		if err != nil {
			fmt.Println("Error Dial: " + err.Error())
		}
	}

	start := time.Now()
	if udpconn != nil {
		_, err = fmt.Fprintf(udpconn, payload+"\r\n")
	}
	if err != nil {
		fmt.Printf("UDP request failed with error: %s\n", err)
		udpconn = nil
	}

	elapsed := time.Since(start)
	resultsChannel <- buildUdpResult(0, 200, elapsed.Nanoseconds(), udpAction.Title)

}

func buildUdpResult(contentLength int, status int, elapsed int64, title string) HttpReqResult {
	httpReqResult := HttpReqResult{
		"UDP",
		elapsed,
		contentLength,
		status,
		title,
		time.Since(SimulationStart).Nanoseconds(),
	}
	return httpReqResult
}

func SubstParams(sessionMap map[string]string, textData string) string {
	if strings.ContainsAny(textData, "${") {
		res := re.FindAllStringSubmatch(textData, -1)
		for _, v := range res {
			textData = strings.Replace(textData, "${"+v[1]+"}", url.QueryEscape(sessionMap[v[1]]), 1)
		}
		return textData
	}
	return textData
}
