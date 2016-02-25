package main

import (
    "io/ioutil"
    "log"
    "math/rand"
    "net/http"
    "strings"
    "time"
    "gopkg.in/xmlpath.v2"
    "github.com/NodePrime/jsonpath"
    "bytes"
)

// Accepts a Httpaction and a one-way channel to write the results to.
func DoHttpRequest(httpAction HttpAction, resultsChannel chan HttpReqResult, sessionMap map[string]string) {
    req := buildHttpRequest(httpAction, sessionMap)
    client := &http.Client{}
    start := time.Now()
    resp, err := client.Do(req)
    if err != nil {
        //log.Printf("HTTP request failed")
    } else {
        elapsed := time.Since(start)
        responseBody, err := ioutil.ReadAll(resp.Body)
        if err != nil {
            //log.Fatal(err)
            log.Printf("Reading HTTP response failed: %s\n", err)
            httpReqResult := buildHttpResult(0, resp.StatusCode, elapsed.Nanoseconds(), httpAction.Title)

            resultsChannel <- httpReqResult
        } else {
            defer resp.Body.Close()
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
    } else {
        req, err = http.NewRequest(httpAction.Method, SubstParams(sessionMap, httpAction.Url), nil)
    }
    if err != nil {
        log.Fatal(err)
    }
    req.Header.Add("Accept", httpAction.Accept)

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
    log.Println("ENTER - processResult")
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
        log.Println("ENTER - xmlPath")
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
            break
        case LAST:
            sessionMap[httpAction.ResponseHandler.Variable] = resultsArray[resultCount-1]
            break
        case RANDOM:
            if resultCount > 1 {
                sessionMap[httpAction.ResponseHandler.Variable] = resultsArray[rand.Intn(resultCount-1)]
            } else {
                sessionMap[httpAction.ResponseHandler.Variable] = resultsArray[0]
            }
            break
        }

    } else {
        // TODO how to handle requested, but missing result?
    }
}
