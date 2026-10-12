package provider

// fakeFacade is an in-memory stand-in for services/svc-openapi: it verifies
// the CPS1 signature with pkg-go/cps1 (the gateway's check), reads Action and
// Version from the query and the parameters from the JSON body, and answers
// with the facade's envelope and error vocabulary. Creates honour ClientToken
// (a repeated token returns the resource it already made); new resources and
// spec changes stay Creating/Modifying for one Describe, deletes Deleting.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qifalab/euler-platform/cps1"
	"github.com/qifalab/euler-platform/eusdk"
)

const (
	testAK     = "EUAKTEST"
	testSK     = "sk-test"
	testRegion = "cn-north-1"
)

type fakeProduct struct {
	idParam, listKey, prefix, ready string
	create, describe, delete        string
	byName                          bool
	nameParam, rename               string
	specParam, specDefault          string
	specAction                      string
	zonal, billed                   bool
	notFound                        string
	extra                           func(params, item map[string]any)
}

var fakeProducts = map[string]*fakeProduct{
	"euecs": {idParam: "InstanceId", listKey: "Instances", prefix: "i-", ready: "Running",
		create: "RunInstances", describe: "DescribeInstances", delete: "TerminateInstances",
		nameParam: "InstanceName", rename: "ModifyInstanceAttribute", specParam: "InstanceType", specAction: "ModifyInstanceSpec",
		zonal: true, billed: true, notFound: "InvalidInstanceId.NotFound"},
	"eurds": {idParam: "InstanceId", listKey: "Instances", prefix: "rds-", ready: "Running",
		create: "CreateInstance", describe: "DescribeInstances", delete: "DeleteInstance",
		nameParam: "InstanceName", rename: "ModifyInstanceAttribute", specParam: "InstanceClass", specDefault: "mysql8.small",
		specAction: "ModifyInstanceSpec", zonal: true, billed: true, notFound: "InvalidInstanceId.NotFound",
		extra: func(p, item map[string]any) {
			item["Engine"] = "MySQL"
			item["EngineVersion"] = p["EngineVersion"]
		}},
	"euvpc": {idParam: "VpcId", listKey: "Vpcs", prefix: "vpc-", ready: "Available",
		create: "CreateVpc", describe: "DescribeVpcs", delete: "DeleteVpc",
		nameParam: "VpcName", rename: "ModifyVpcAttribute", notFound: "InvalidVpcId.NotFound",
		extra: func(p, item map[string]any) { item["CidrBlock"] = p["CidrBlock"] }},
	"eubs": {idParam: "DiskId", listKey: "Disks", prefix: "d-", ready: "Available",
		create: "CreateDisk", describe: "DescribeDisks", delete: "DeleteDisk",
		nameParam: "DiskName", rename: "ModifyDiskAttribute", specParam: "DiskCategory", specDefault: "essd",
		zonal: true, billed: true, notFound: "InvalidDiskId.NotFound",
		extra: func(p, item map[string]any) { item["Size"] = p["Size"] }},
	"eueip": {idParam: "AllocationId", listKey: "EipAddresses", prefix: "eip-", ready: "Available",
		create: "AllocateEipAddress", describe: "DescribeEipAddresses", delete: "ReleaseEipAddress",
		nameParam: "Name", zonal: true, billed: true, notFound: "InvalidAllocationId.NotFound",
		extra: func(p, item map[string]any) {
			item["Bandwidth"] = json.Number("5")
			if bw, ok := p["Bandwidth"]; ok {
				item["Bandwidth"] = bw
			}
			item["InternetChargeType"] = "PayByTraffic"
		}},
	"euoss": {idParam: "Bucket", listKey: "Buckets", ready: "Available",
		create: "CreateBucket", describe: "ListBuckets", delete: "DeleteBucket", byName: true,
		notFound: "InvalidBucketName.NotFound",
		extra:    func(p, item map[string]any) { item["StorageClass"] = "Standard" }},
}

type fakeCall struct {
	product, action string
	params          map[string]any
}

type fakeFacade struct {
	t  *testing.T
	mu sync.Mutex
	// items[product][id] is the Describe view of a live resource.
	items    map[string]map[string]map[string]any
	pending  map[string]string // id -> status reached after the next Describe
	deleting map[string]bool
	tokens   map[string]string // product/token -> id
	seq      int
	calls    []fakeCall
	// hook answers a call itself (status, raw body) when handled.
	hook func(product, action string, params map[string]any) (status int, body string, handled bool)
	// dropCreates answers that many creates with 503 after committing them —
	// the response lost on the way back.
	dropCreates int
	// createStatus is the status a new resource settles into ("" = ready).
	createStatus string
}

func newFakeFacade(t *testing.T) (*fakeFacade, *apiClient) {
	t.Helper()
	f := &fakeFacade{t: t, items: map[string]map[string]map[string]any{}, pending: map[string]string{},
		deleting: map[string]bool{}, tokens: map[string]string{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, newAPIClient(eusdk.Config{AK: testAK, SK: testSK, Region: testRegion, Endpoint: srv.URL})
}

func (f *fakeFacade) callsOf(action string) []fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []fakeCall
	for _, c := range f.calls {
		if c.action == action {
			out = append(out, c)
		}
	}
	return out
}

func (f *fakeFacade) item(product, id string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.items[product][id]
}

func (f *fakeFacade) count(product string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.items[product])
}

func writeEnvelope(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func (f *fakeFacade) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	rid := fmt.Sprintf("req-%d", time.Now().UnixNano())
	fail := func(status int, code, msg string) {
		writeEnvelope(w, status, map[string]any{"RequestId": rid, "Code": code, "Message": msg})
	}
	// The gateway's signature check: the scope names the service the
	// client signed for (euecs → ecs).
	headers := map[string]string{"host": r.Host}
	for k, vs := range r.Header {
		headers[strings.ToLower(k)] = vs[0]
	}
	auth := headers["authorization"]
	i := strings.Index(auth, "Credential=")
	var scope []string
	if i >= 0 {
		scope = strings.Split(strings.SplitN(auth[i+len("Credential="):], ",", 2)[0], "/")
	}
	if len(scope) != 5 {
		f.t.Errorf("request without a CPS1 credential scope: %q", auth)
		fail(http.StatusForbidden, "IAM.SignatureDoesNotMatch", "no credential scope")
		return
	}
	service := scope[3]
	if err := cps1.Verify(cps1.Request{Method: r.Method, Host: r.Host, Path: r.URL.Path, Query: r.URL.Query(),
		Headers: headers, Body: body}, cps1.Credentials{AK: testAK, SK: testSK}, testRegion, service, time.Now()); err != nil {
		f.t.Errorf("signature check failed: %v", err)
		fail(http.StatusForbidden, "IAM.SignatureDoesNotMatch", err.Error())
		return
	}
	product := "eu" + service
	q := r.URL.Query()
	action := q.Get("Action")
	if q.Get("Version") != apiVersion || r.URL.Path != "/" || r.Method != http.MethodPost {
		f.t.Errorf("request %s %s?%s, want POST / with Version %s", r.Method, r.URL.Path, r.URL.RawQuery, apiVersion)
	}
	params := map[string]any{}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&params); err != nil {
		fail(http.StatusBadRequest, "InvalidParameter", "The JSON request body is malformed.")
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fakeCall{product: product, action: action, params: params})
	if f.hook != nil {
		if status, raw, handled := f.hook(product, action, params); handled {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = io.WriteString(w, raw)
			return
		}
	}
	data, status, code, msg := f.serve(product, action, params)
	if code != "" {
		fail(status, code, msg)
		return
	}
	writeEnvelope(w, http.StatusOK, map[string]any{"RequestId": rid, "Code": "OK", "Data": data})
}

func sval(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}

// serve runs one Action under f.mu.
func (f *fakeFacade) serve(product, action string, p map[string]any) (data map[string]any, status int, code, msg string) {
	fp := fakeProducts[product]
	if fp == nil {
		return nil, http.StatusBadRequest, "InvalidProduct.NotFound", product
	}
	if f.items[product] == nil {
		f.items[product] = map[string]map[string]any{}
	}
	items := f.items[product]
	id := sval(p[fp.idParam])
	switch action {
	case fp.create:
		token := sval(p["ClientToken"])
		if existing, ok := f.tokens[product+"/"+token]; ok && token != "" {
			if f.dropCreates > 0 {
				f.dropCreates--
				return nil, http.StatusServiceUnavailable, "ServiceUnavailable", "The request has failed due to a temporary failure of the server."
			}
			return map[string]any{fp.idParam: existing, "OrderId": "o-" + existing, "Status": "Creating"}, 0, "", ""
		}
		if fp.specParam != "" && sval(p[fp.specParam]) == "" {
			if fp.specDefault == "" {
				return nil, http.StatusBadRequest, "MissingParameter", "The input parameter " + fp.specParam + " that is mandatory for processing this request is not supplied."
			}
			p[fp.specParam] = fp.specDefault
		}
		f.seq++
		id = fmt.Sprintf("%s%d", fp.prefix, f.seq)
		if fp.byName {
			id = sval(p["Bucket"])
			if _, taken := items[id]; taken {
				return nil, http.StatusConflict, "BucketAlreadyExists", "The requested bucket name is not available."
			}
		}
		region := sval(p["RegionId"])
		if region == "" {
			region = "cn-east-1" // the deployment default: the provider must send its own
		}
		item := map[string]any{fp.idParam: id, "RegionId": region, "Status": "Creating", "CreationTime": "2026-10-09T10:00:00Z"}
		if fp.nameParam != "" {
			item[fp.nameParam] = sval(p[fp.nameParam])
		}
		if fp.zonal {
			zone := sval(p["ZoneId"])
			if zone == "" {
				zone = region + "-a"
			}
			item["ZoneId"] = zone
		}
		if fp.specParam != "" {
			item[fp.specParam] = strings.ToLower(sval(p[fp.specParam]))
		}
		if fp.billed {
			ct := "Postpaid"
			if strings.EqualFold(sval(p["ChargeType"]), "prepaid") {
				ct = "Prepaid"
			}
			item["ChargeType"] = ct
			item["SkuCode"] = product + "." + nonEmpty(sval(item[fp.specParam]), "traffic") + "." + strings.ToLower(ct)
			item["ExpiredTime"] = ""
		}
		if fp.extra != nil {
			fp.extra(p, item)
		}
		items[id] = item
		f.pending[id] = nonEmpty(f.createStatus, fp.ready)
		f.tokens[product+"/"+token] = id
		if f.dropCreates > 0 {
			f.dropCreates--
			return nil, http.StatusServiceUnavailable, "ServiceUnavailable", "The request has failed due to a temporary failure of the server."
		}
		out := map[string]any{fp.idParam: id, "OrderId": "o-" + id, "Status": "Creating"}
		if fp.byName {
			out["BucketName"] = id
			out["ResourceId"] = "oss-" + id
		}
		return out, 0, "", ""

	case fp.describe:
		var list []map[string]any
		var ids []string
		for k := range items {
			ids = append(ids, k)
		}
		sort.Strings(ids)
		for _, k := range ids {
			if fp.byName && !strings.HasPrefix(k, sval(p["Prefix"])) || !fp.byName && id != "" && k != id {
				continue
			}
			list = append(list, f.view(product, k))
		}
		number, size := 1, 10
		fmt.Sscan(nonEmpty(sval(p["PageNumber"]), "1"), &number)
		fmt.Sscan(nonEmpty(sval(p["PageSize"]), "10"), &size)
		total := len(list)
		from, to := min((number-1)*size, total), min(number*size, total)
		page := list[from:to]
		if page == nil {
			page = []map[string]any{}
		}
		return map[string]any{fp.listKey: page, "TotalCount": total, "PageNumber": number, "PageSize": size}, 0, "", ""

	case fp.delete:
		if items[id] == nil {
			return nil, http.StatusNotFound, fp.notFound, "The specified " + fp.idParam + " does not exist."
		}
		items[id]["Status"] = "Deleting"
		f.deleting[id] = true
		return map[string]any{fp.idParam: id}, 0, "", ""

	case fp.rename:
		if items[id] == nil {
			return nil, http.StatusNotFound, fp.notFound, "The specified " + fp.idParam + " does not exist."
		}
		name := sval(p[fp.nameParam])
		if name == "" {
			return nil, http.StatusBadRequest, "MissingParameter", fp.nameParam
		}
		items[id][fp.nameParam] = name
		return map[string]any{fp.idParam: id, fp.nameParam: name}, 0, "", ""

	case fp.specAction:
		if items[id] == nil {
			return nil, http.StatusNotFound, fp.notFound, "The specified " + fp.idParam + " does not exist."
		}
		spec := strings.ToLower(sval(p[fp.specParam]))
		items[id][fp.specParam] = spec
		items[id]["SkuCode"] = product + "." + spec + ".postpaid"
		items[id]["Status"] = "Modifying"
		f.pending[id] = fp.ready
		return map[string]any{fp.idParam: id, "SkuCode": items[id]["SkuCode"], "Status": "Modifying"}, 0, "", ""
	}
	return nil, http.StatusBadRequest, "InvalidAction.NotFound", fmt.Sprintf("The specified action %q is not supported by %s.", action, product)
}

// view renders a resource for Describe and advances its lifecycle: a pending
// status is reported once, a deleting resource disappears after being seen.
func (f *fakeFacade) view(product, id string) map[string]any {
	item := f.items[product][id]
	out := make(map[string]any, len(item))
	for k, v := range item {
		out[k] = v
	}
	if next, ok := f.pending[id]; ok {
		item["Status"] = next
		delete(f.pending, id)
	}
	if f.deleting[id] {
		delete(f.items[product], id)
		delete(f.deleting, id)
	}
	return out
}

func nonEmpty(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
