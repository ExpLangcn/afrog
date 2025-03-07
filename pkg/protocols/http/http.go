package http

import (
	"bytes"
	"crypto/tls"
	"errors"
	"fmt"
	"github.com/zan8in/afrog/pkg/utils"
	"io/ioutil"
	"net/http"
	"net/url"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/valyala/fasthttp"
	"github.com/valyala/fasthttp/fasthttpproxy"
	"github.com/zan8in/afrog/pkg/config"
	"github.com/zan8in/afrog/pkg/log"
	"github.com/zan8in/afrog/pkg/poc"
	"github.com/zan8in/afrog/pkg/proto"
)

var (
	F = &fasthttp.Client{}
)

type FastClient struct {
	MaxRedirect int32
	DialTimeout int32
	UserAgent   string
}

func Init(options *config.Options) {
	readTimeout, _ := time.ParseDuration(options.Config.ConfigHttp.ReadTimeout)
	writeTimeout, _ := time.ParseDuration(options.Config.ConfigHttp.WriteTimeout)
	maxIdleConnDuration, _ := time.ParseDuration(options.Config.ConfigHttp.MaxIdle)
	F = &fasthttp.Client{
		TLSConfig:                     &tls.Config{InsecureSkipVerify: true},
		MaxConnsPerHost:               options.Config.ConfigHttp.MaxConnsPerHost, // 每个主机的最大空闲连接数
		ReadTimeout:                   readTimeout,
		WriteTimeout:                  writeTimeout,
		MaxIdleConnDuration:           maxIdleConnDuration,
		NoDefaultUserAgentHeader:      true, // Don't send: User-Agent: fasthttp
		DisableHeaderNamesNormalizing: true, // If you set the case on your headers correctly you can enable this
		DisablePathNormalizing:        true,
		MaxResponseBodySize:           options.Config.ConfigHttp.MaxResponseBodySize, // 2m
		// increase DNS cache time to an hour instead of default minute
		Dial: (&fasthttp.TCPDialer{
			Concurrency:      options.Config.ConfigHttp.Concurrency,
			DNSCacheDuration: time.Hour,
		}).Dial,
	}
	if len(strings.TrimSpace(options.Config.ConfigHttp.Proxy)) > 0 {
		// client.Dial = fasthttpproxy.FasthttpHTTPDialerTimeout("localhost:10808", time.Second*5) // http proxy 有问题，不支持https访问
		F.Dial = fasthttpproxy.FasthttpSocksDialer("socks5://" + options.Config.ConfigHttp.Proxy)
	}
}

func (fc *FastClient) HTTPRequest(httpRequest *http.Request, rule poc.Rule, variableMap map[string]interface{}) error {
	var err error

	protoRequest, err := ParseRequest(httpRequest)
	if err != nil {
		return err
	}

	variableMap["request"] = nil
	variableMap["response"] = nil

	newRuleHeader := make(map[string]string)
	for k, v := range rule.Request.Headers {
		newRuleHeader[k] = fc.AssignVariableMap(v, variableMap)
	}
	rule.Request.Headers = newRuleHeader
	rule.Request.Path = fc.AssignVariableMap(strings.TrimSpace(rule.Request.Path), variableMap)
	//rule.Request.Body = fc.AssignVariableMap(strings.TrimSpace(rule.Request.Body), variableMap)
	if strings.HasPrefix(strings.ToLower(rule.Request.Headers["Content-Type"]), "multipart/form-Data") && strings.Contains(rule.Request.Body, "\n\n") {
		multipartBody, err := DealMultipart(rule.Request.Headers["Content-Type"], rule.Request.Body)
		if err != nil {
			return err
		}
		rule.Request.Body = fc.AssignVariableMap(strings.TrimSpace(multipartBody), variableMap)
	} else {
		rule.Request.Body = fc.AssignVariableMap(strings.TrimSpace(rule.Request.Body), variableMap)
	}

	if strings.HasPrefix(rule.Request.Path, "/") {
		protoRequest.Url.Path = strings.TrimRight(httpRequest.URL.Path, "/") + rule.Request.Path
	} else if strings.HasPrefix(rule.Request.Path, "^") {
		protoRequest.Url.Path = "/" + rule.Request.Path[1:]
	}

	protoRequest.Url.Path = strings.ReplaceAll(protoRequest.Url.Path, " ", "%20")
	protoRequest.Url.Path = strings.ReplaceAll(protoRequest.Url.Path, "+", "%20")

	if rule.Request.Method == "POST" && len(rule.Request.Headers["Content-Type"]) == 0 {
		if rule.Request.Headers == nil {
			rule.Request.Headers = make(map[string]string)
		}
		rule.Request.Headers["Content-Type"] = "application/x-www-form-urlencoded"
	}

	finalRequest, err := http.NewRequest(rule.Request.Method, fmt.Sprintf("%s://%s%s", protoRequest.Url.Scheme, protoRequest.Url.Host, protoRequest.Url.Path), strings.NewReader(rule.Request.Body))
	if err != nil {
		return err
	}

	fastReq := fasthttp.AcquireRequest()
	defer fasthttp.ReleaseRequest(fastReq)

	var rawHeader strings.Builder
	newheader := make(map[string]string)
	for k, v := range rule.Request.Headers {
		fastReq.Header.Set(k, v)

		newheader[k] = v

		rawHeader.WriteString(k)
		rawHeader.WriteString(": ")
		rawHeader.WriteString(v)
		rawHeader.WriteString("\n")
	}

	if len(fc.UserAgent) > 0 {
		fastReq.Header.Set("User-Agent", fc.UserAgent)
	} else {
		fastReq.Header.Set("User-Agent", utils.RandomUA())
	}

	fastReq.Header.SetMethod(rule.Request.Method)

	if len(rule.Request.Headers["Host"]) == 0 {
		fastReq.Header.SetHost(finalRequest.Host)
	}
	if len(rule.Request.Headers["Content-Length"]) == 0 {
		fastReq.Header.SetContentLength(int(finalRequest.ContentLength))
	}
	fastReq.SetBody([]byte(rule.Request.Body))
	fastReq.URI().Update(finalRequest.URL.String())
	fastReq.SetRequestURI(finalRequest.URL.String())

	fastResp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseResponse(fastResp)

	// 新增功能：HTTP请求超时，自动重连机制（3次，每次累加超时时间）
	repeatCount := 0
	for {
		if rule.Request.FollowRedirects {
			maxrd := 5 // follow redirects default 5
			if fc.MaxRedirect > 0 {
				maxrd = int(fc.MaxRedirect)
			}
			err = F.DoRedirects(fastReq, fastResp, maxrd)
		} else {
			dialtimeout := 6
			if fc.DialTimeout > 0 {
				dialtimeout = int(fc.DialTimeout)
			}
			if repeatCount > 0 {
				dialtimeout = dialtimeout + dialtimeout*repeatCount
			}
			err = F.DoTimeout(fastReq, fastResp, time.Second*time.Duration(dialtimeout))
		}
		if err != nil {
			errName, known := httpConnError(err)
			if known {
				log.Log().Error(fmt.Sprintf("WARN conn error: %s\n", errName))
			} else {
				log.Log().Error(fmt.Sprintf("ERR conn failure: %s %s\n", errName, err))
			}
			if errName == "timeout" {
				repeatCount++
				if repeatCount > 1 {
					break
				}
			} else {
				break
			}
		}
		if err == nil {
			break
		}
	}

	// set fastResp body
	var respBody []byte
	contentEncoding := strings.ToLower(string(fastResp.Header.Peek("Content-Encoding")))
	switch contentEncoding {
	case "", "none", "identity":
		respBody = fastResp.Body()
	case "gzip":
		respBody, err = fastResp.BodyGunzip()
	case "deflate":
		respBody, err = fastResp.BodyInflate()
	default:
		respBody = []byte{}
	}
	if err != nil {
		return err
	}
	fastResp.SetBody(respBody)

	// fc.VariableMap["response"] variable assignment
	tempResultResponse := &proto.Response{}
	tempResultResponse.Status = int32(fastResp.StatusCode())
	u, err := url.Parse(fastReq.URI().String())
	if err != nil {
		return err
	}
	urlType := &proto.UrlType{
		Scheme:   u.Scheme,
		Domain:   u.Hostname(),
		Host:     u.Host,
		Port:     u.Port(),
		Path:     u.Path,
		Query:    u.RawQuery,
		Fragment: u.Fragment,
	}
	tempResultResponse.Url = urlType
	newheader2 := make(map[string]string)
	respHeaderSlice := strings.Split(fastResp.Header.String(), "\r\n")
	for _, h := range respHeaderSlice {
		hslice := strings.SplitN(h, ":", 2)
		if len(hslice) != 2 {
			continue
		}
		k := strings.ToLower(hslice[0])
		v := strings.TrimLeft(hslice[1], " ")
		if newheader2[k] != "" {
			newheader2[k] += v
		} else {
			newheader2[k] = v
		}
	}
	tempResultResponse.Headers = newheader2
	tempResultResponse.ContentType = string(fastResp.Header.ContentType())
	tempResultResponse.Body = fastResp.Body()
	tempResultResponse.Raw = []byte(fastResp.String())
	tempResultResponse.RawHeader = fastResp.Header.Header()
	variableMap["response"] = tempResultResponse

	protoRequest.Method = rule.Request.Method
	protoRequest.Url = urlType
	protoRequest.RawHeader = []byte(strings.Trim(rawHeader.String(), "\n"))
	protoRequest.Raw = []byte(string(fastReq.Header.String()) + "\n" + string(fastReq.Body()))
	protoRequest.Headers = newheader
	protoRequest.ContentType = newheader["content-type"]
	protoRequest.Body = []byte(rule.Request.Body)
	variableMap["request"] = protoRequest

	return err
}

func (fc *FastClient) HTTPRequest2(httpRequest *http.Request, rule poc.Rule, variableMap map[string]interface{}) error {
	var err error

	variableMap["request"] = nil
	variableMap["response"] = nil

	fastReq := fasthttp.AcquireRequest()
	defer fasthttp.ReleaseRequest(fastReq)

	CopyRequest(httpRequest, fastReq, nil)

	// set fastReq.Header from poc.Rule
	fastReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for k, v := range rule.Request.Headers {
		fastReq.Header.Set(k, fc.AssignVariableMap(v, variableMap))
	}

	// set fastReq.Header method from poc.Rule
	fastReq.Header.SetMethod(rule.Request.Method)

	// set fastReq Path from poc.Rule
	tempPath := ""
	if strings.HasPrefix(rule.Request.Path, "/") {
		tempPath = strings.TrimRight(httpRequest.URL.Path, "/") + rule.Request.Path // 如果 path 是以 / 开头的， 取 dir 路径拼接
	} else if strings.HasPrefix(rule.Request.Path, "^") {
		tempPath = "/" + rule.Request.Path[1:] // 如果 path 是以 ^ 开头的， uri 直接取该路径
	} else {
		return errors.New("poc rule request path format err, prefix no `/`")
	}
	tempPath = strings.ReplaceAll(tempPath, " ", "%20")
	tempPath = strings.ReplaceAll(tempPath, "+", "%20")
	tempPath = fc.AssignVariableMap(strings.TrimSpace(tempPath), variableMap)
	fastReq.URI().Update(tempPath)
	fastReq.SetRequestURI(httpRequest.URL.String() + tempPath) // fixed no such host error.

	// set fastReq Body from poc.Rule
	contentType := string(fastReq.Header.ContentType())
	if strings.HasPrefix(strings.ToLower(contentType), "multipart/form-Data") && strings.Contains(rule.Request.Body, "\n\n") {
		multipartBody, err := DealMultipart(contentType, rule.Request.Body)
		if err != nil {
			return err
		}
		fastReq.SetBody([]byte(fc.AssignVariableMap(strings.TrimSpace(multipartBody), variableMap)))
	} else {
		fastReq.SetBody([]byte(fc.AssignVariableMap(strings.TrimSpace(rule.Request.Body), variableMap)))
	}

	fastResp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseResponse(fastResp)

	if rule.Request.FollowRedirects {
		maxrd := 5 // follow redirects default 5
		if fc.MaxRedirect > 0 {
			maxrd = int(fc.MaxRedirect)
		}
		err = F.DoRedirects(fastReq, fastResp, maxrd)
	} else {
		dialtimeout := 6
		if fc.DialTimeout > 0 {
			dialtimeout = int(fc.DialTimeout)
		}
		err = F.DoTimeout(fastReq, fastResp, time.Second*time.Duration(dialtimeout))
	}
	if err != nil {
		errName, known := httpConnError(err)
		if known {
			log.Log().Error(fmt.Sprintf("WARN conn error: %s\n", errName))
		} else {
			log.Log().Error(fmt.Sprintf("ERR conn failure: %s %s\n", errName, err))
		}
	}

	// set fastResp body
	var respBody []byte
	contentEncoding := strings.ToLower(string(fastResp.Header.Peek("Content-Encoding")))
	switch contentEncoding {
	case "", "none", "identity":
		respBody = fastResp.Body()
	case "gzip":
		respBody, err = fastResp.BodyGunzip()
	case "deflate":
		respBody, err = fastResp.BodyInflate()
	default:
		respBody = []byte{}
	}
	if err != nil {
		return err
	}
	fastResp.SetBody(respBody)

	// fc.VariableMap["response"] variable assignment
	tempResultResponse := AcquireProtoResponsePool()
	tempResultResponse.Status = int32(fastResp.StatusCode())
	u, err := url.Parse(fastReq.URI().String())
	if err != nil {
		return err
	}
	urlType := &proto.UrlType{
		Scheme:   u.Scheme,
		Domain:   u.Hostname(),
		Host:     u.Host,
		Port:     u.Port(),
		Path:     u.Path,
		Query:    u.RawQuery,
		Fragment: u.Fragment,
	}
	tempResultResponse.Url = urlType
	newheader := make(map[string]string)
	respHeaderSlice := strings.Split(fastResp.Header.String(), "\r\n")
	for _, h := range respHeaderSlice {
		hslice := strings.SplitN(h, ":", 2)
		if len(hslice) != 2 {
			continue
		}
		k := strings.ToLower(hslice[0])
		v := strings.TrimLeft(hslice[1], " ")
		if newheader[k] != "" {
			newheader[k] += v
		} else {
			newheader[k] = v
		}
	}
	tempResultResponse.Headers = newheader
	tempResultResponse.ContentType = string(fastResp.Header.ContentType())
	tempResultResponse.Body = fastResp.Body()
	tempResultResponse.Raw = []byte(fastResp.String())
	tempResultResponse.RawHeader = fastResp.Header.Header()
	// tempResultResponse.Conn.Source.Addr = fastResp.LocalAddr().String()
	// tempResultResponse.Conn.Destination.Addr = fastResp.RemoteAddr().String()
	variableMap["response"] = tempResultResponse

	// fc.VariableMap["request"] variable assignment
	tempResultRequest := AcquireProtoRequestPool()
	tempResultRequest.Method = string(fastReq.Header.Method())
	tempResultRequest.Url = urlType
	newReqheader := make(map[string]string)
	reqHeaderSlice := strings.Split(fastReq.Header.String(), "\r\n")
	for _, h := range reqHeaderSlice {
		hslice := strings.SplitN(h, ":", 2)
		if len(hslice) != 2 {
			continue
		}
		k := strings.ToLower(hslice[0])
		v := strings.TrimLeft(hslice[1], " ")
		if newReqheader[k] != "" {
			newReqheader[k] += v
		} else {
			newReqheader[k] = v
		}
	}
	tempResultRequest.Headers = newReqheader
	tempResultRequest.ContentType = newReqheader["content-type"]
	tempResultRequest.Body = fastReq.Body()
	tempResultRequest.RawHeader = fastReq.Header.Header()
	tempResultRequest.Raw = []byte(string(fastReq.Header.Header()) + string(fastReq.Body()))
	variableMap["request"] = tempResultRequest

	return err
}

// reverse http request
func (fc *FastClient) SampleHTTPRequest(httpRequest *http.Request) (*proto.Response, error) {
	var err error
	tempResultResponse := AcquireProtoResponsePool()

	fastReq := fasthttp.AcquireRequest()
	defer fasthttp.ReleaseRequest(fastReq)

	CopyRequest(httpRequest, fastReq, nil)

	fastResp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseResponse(fastResp)

	err = F.DoTimeout(fastReq, fastResp, time.Second*6)
	if err != nil {
		errName, known := httpConnError(err)
		if known {
			log.Log().Error(fmt.Sprintf("WARN conn error: %s\n", errName))
		} else {
			log.Log().Error(fmt.Sprintf("ERR conn failure: %s %s\n", errName, err))
		}
	}

	// set fastResp body
	var respBody []byte
	contentEncoding := strings.ToLower(string(fastResp.Header.Peek("Content-Encoding")))
	switch contentEncoding {
	case "", "none", "identity":
		respBody = fastResp.Body()
	case "gzip":
		respBody, err = fastResp.BodyGunzip()
	case "deflate":
		respBody, err = fastResp.BodyInflate()
	default:
		respBody = []byte{}
	}
	if err != nil {
		return tempResultResponse, err
	}
	fastResp.SetBody(respBody)

	// fc.VariableMap["response"] variable assignment
	tempResultResponse.Status = int32(fastResp.StatusCode())
	u, err := url.Parse(fastReq.URI().String())
	if err != nil {
		return tempResultResponse, err
	}
	urlType := &proto.UrlType{
		Scheme:   u.Scheme,
		Domain:   u.Hostname(),
		Host:     u.Host,
		Port:     u.Port(),
		Path:     u.Path,
		Query:    u.RawQuery,
		Fragment: u.Fragment,
	}
	tempResultResponse.Url = urlType
	newheader := make(map[string]string)
	respHeaderSlice := strings.Split(fastResp.Header.String(), "\r\n")
	for _, h := range respHeaderSlice {
		hslice := strings.SplitN(h, ":", 2)
		if len(hslice) != 2 {
			continue
		}
		k := strings.ToLower(hslice[0])
		v := strings.TrimLeft(hslice[1], " ")
		if newheader[k] != "" {
			newheader[k] += v
		} else {
			newheader[k] = v
		}
	}
	tempResultResponse.Headers = newheader
	tempResultResponse.ContentType = string(fastResp.Header.ContentType())
	tempResultResponse.Body = fastResp.Body()
	tempResultResponse.Raw = []byte(fastResp.String())
	tempResultResponse.RawHeader = fastResp.Header.Header()

	return tempResultResponse, err
}

func ReverseHttpRequest(httpRequest *http.Request) ([]byte, error) {
	var err error

	fastReq := fasthttp.AcquireRequest()
	defer fasthttp.ReleaseRequest(fastReq)

	CopyRequest(httpRequest, fastReq, nil)

	fastResp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseResponse(fastResp)

	err = F.DoTimeout(fastReq, fastResp, time.Second*30)
	if err != nil {
		errName, known := httpConnError(err)
		if known {
			log.Log().Error(fmt.Sprintf("WARN conn error: %s\n", errName))
		} else {
			log.Log().Error(fmt.Sprintf("ERR conn failure: %s %s\n", errName, err))
		}
	}

	return fastResp.Body(), err
}

func GetTitleRedirect(httpRequest *http.Request, redirect int) ([]byte, int, error) {
	var err error

	fastReq := fasthttp.AcquireRequest()
	defer fasthttp.ReleaseRequest(fastReq)

	CopyRequest(httpRequest, fastReq, nil)

	fastResp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseResponse(fastResp)

	err = F.DoRedirects(fastReq, fastResp, redirect)
	if err != nil {
		errName, known := httpConnError(err)
		if known {
			log.Log().Error(fmt.Sprintf("WARN conn error: %s\n", errName))
		} else {
			log.Log().Error(fmt.Sprintf("ERR conn failure: %s %s\n", errName, err))
		}
	}

	return fastResp.Body(), fastResp.StatusCode(), err
}

func httpConnError(err error) (string, bool) {
	errName := ""
	known := false
	if err == fasthttp.ErrTimeout {
		errName = "timeout"
		known = true
	} else if err == fasthttp.ErrNoFreeConns {
		errName = "conn_limit"
		known = true
	} else if err == fasthttp.ErrConnectionClosed {
		errName = "conn_close"
		known = true
	} else {
		errName = reflect.TypeOf(err).String()
		if errName == "*net.OpError" {
			// Write and Read errors are not so often and in fact they just mean timeout problems
			errName = "timeout"
			known = true
		}
	}
	return errName, known
}

func DealMultipart(contentType string, ruleBody string) (result string, err error) {
	// 处理multipart的/n
	re := regexp.MustCompile(`(?m)multipart\/form-Data; boundary=(.*)`)
	match := re.FindStringSubmatch(contentType)
	if len(match) != 2 {
		return "", errors.New("no boundary in content-type")
	}
	boundary := "--" + match[1]

	// 处理rule
	multiPartContent := ""
	multiFile := strings.Split(ruleBody, boundary)
	if len(multiFile) == 0 {
		return multiPartContent, errors.New("ruleBody.Body multi content format err")
	}

	for _, singleFile := range multiFile {
		//	处理单个文件
		//	文件头和文件响应
		spliteTmp := strings.Split(singleFile, "\n\n")
		if len(spliteTmp) == 2 {
			fileHeader := spliteTmp[0]
			fileBody := spliteTmp[1]
			fileHeader = strings.Replace(fileHeader, "\n", "\r\n", -1)
			multiPartContent += boundary + fileHeader + "\r\n\r\n" + strings.TrimRight(fileBody, "\n") + "\r\n"
		}
	}
	multiPartContent += boundary + "--" + "\r\n"
	return multiPartContent, nil
}

func (fc *FastClient) AssignVariableMap(find string, variableMap map[string]interface{}) string {
	for k, v := range variableMap {
		_, isMap := v.(map[string]string)
		if isMap {
			continue
		}
		newstr := fmt.Sprintf("%v", v)
		oldstr := "{{" + k + "}}"
		if !strings.Contains(find, oldstr) {
			continue
		}
		find = strings.ReplaceAll(find, oldstr, newstr)
	}
	return find
}

func CopyRequest(req *http.Request, dstRequest *fasthttp.Request, data []byte) {
	//dstRequest.SetRequestURI(req.URL.String())
	dstRequest.URI().Update(req.URL.String())
	dstRequest.Header.SetMethod(req.Method)
	for name, values := range req.Header {
		// Loop over all values for the name.
		for index, value := range values {
			if index > 0 {
				dstRequest.Header.Add(name, value)
			} else {
				dstRequest.Header.Set(name, value)
			}
		}
	}
	dstRequest.SetBody(data)
	dstRequest.SetBodyRaw(data)
}

func Url2UrlType(u *url.URL) *proto.UrlType {
	return &proto.UrlType{
		Scheme:   u.Scheme,
		Domain:   u.Hostname(),
		Host:     u.Host,
		Port:     u.Port(),
		Path:     u.EscapedPath(),
		Query:    u.RawQuery,
		Fragment: u.Fragment,
	}
}

var protoRequestPool sync.Pool = sync.Pool{
	New: func() interface{} {
		return new(proto.Request)
	},
}

var protoResponsePool sync.Pool = sync.Pool{
	New: func() interface{} {
		return new(proto.Response)
	},
}

func AcquireProtoRequestPool() *proto.Request {
	return protoRequestPool.Get().(*proto.Request)
}

func ReleaseProtoRequestPool(req *proto.Request) {
	if req != nil {
		req.Reset()
		protoRequestPool.Put(req)
	}
}

func AcquireProtoResponsePool() *proto.Response {
	return protoResponsePool.Get().(*proto.Response)
}

func ReleaseProtoResponsePool(rsp *proto.Response) {
	if rsp != nil {
		rsp.Reset()
		protoResponsePool.Put(rsp)
	}
}

func ParseRequest(oReq *http.Request) (*proto.Request, error) {
	req := &proto.Request{}
	req.Method = oReq.Method
	req.Url = Url2UrlType(oReq.URL)
	header := make(map[string]string)
	for k := range oReq.Header {
		header[k] = oReq.Header.Get(k)
	}
	req.Headers = header
	req.ContentType = oReq.Header.Get("Content-Type")
	if oReq.Body == nil || oReq.Body == http.NoBody {
	} else {
		data, err := ioutil.ReadAll(oReq.Body)
		if err != nil {
			return nil, err
		}
		req.Body = data
		oReq.Body = ioutil.NopCloser(bytes.NewBuffer(data))
	}
	return req, nil
}

func (fc *FastClient) Reset() {
	*fc = FastClient{}
}
