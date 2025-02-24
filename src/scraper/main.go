package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/gob"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"
	"golang.org/x/sync/semaphore"
)

var CENTER_NAMES = []string{
	// "WAC",
	"EAC",
	// "MSC",
	// "LIN",
	// "SRC",
	// "IOE",
	// "YSC" seems nobody cares,
}

var FORM_TYPES = []string{
	"I-129CW",
	"I-129F",
	"I-600A",
	"I-601A",
	"I-765V",
	"I-485J",
	"I-800A",
	"I-821D",
	"I-90",
	"I-102",
	"I-129",
	"I-130",
	"I-131",
	"I-140",
	"I-212",
	"I-360",
	"I-485",
	"I-526",
	"I-539",
	"I-600",
	"I-601",
	"I-612",
	"I-730",
	"I-751",
	"I-765",
	"I-800",
	"I-817",
	"I-821",
	"I-824",
	"I-829",
	"I-914",
	"I-918",
	"I-924",
	"I-929",
}

var FYs = []int{
	23,
}

type Result struct {
	Status string
	Form   string
	CaseID string
}

type RawStorage struct {
	Index  map[Result]int
	Status map[string]int
}

var case_status_store = make(map[string]int)
var case_status_index_store = make(map[Result]int)
var case_form_type_global_cache = make(map[string]string)
var case_status_index = 0
var report_freq int64 = 10000

var case_results = make([]Result, 0)

const (
	center_year_day_code_serial = iota
	center_year_code_day_serial
)

var mutex sync.Mutex
var case_status_store_mutex sync.Mutex
var case_form_store_mutex sync.Mutex
var epoch_day = time.Now().Unix() / 86400
var sem = semaphore.NewWeighted(900)

var start_epoch = time.Now().Unix()
var last_record = start_epoch
var client = &http.Client{
	Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
}

func get(form url.Values, retry int) Result {
	caseID := form.Get("appReceiptNum")
	sem.Acquire(context.Background(), 1)
	res, err1 := client.PostForm("https://egov.uscis.gov/casestatus/mycasestatus.do", form)

	sem.Release(1)

	defer func() {
		if err1 == nil {
			res.Body.Close()
		}
	}()
	if err1 != nil {
		fmt.Println("error 1! " + err1.Error() + "\n")
		fmt.Printf("Retry %d %s\n", retry, form)
		return get(form, retry+1)
	}

	doc, err2 := goquery.NewDocumentFromReader(res.Body)
	if err2 != nil {
		fmt.Println("error 2! " + err2.Error() + "\n")
		fmt.Printf("Retry %d %s\n", retry, form)
		return get(form, retry+1)
	}

	body := doc.Find(".rows").First()
	status := body.Find("h1").Text()
	case_id := form.Get("appReceiptNum")
	for _, form := range FORM_TYPES {
		if strings.Contains(doc.Text(), form) {
			case_form_store_mutex.Lock()
			case_form_type_global_cache[case_id] = form
			case_form_store_mutex.Unlock()
			return Result{status, form, caseID}
		}
	}
	if status != "" {
		case_form_store_mutex.Lock()
		cachedForm, ok := case_form_type_global_cache[case_id]
		case_form_store_mutex.Unlock()
		if ok {
			return Result{status, cachedForm, caseID}
		} else {
			return Result{status, "unknown", caseID}
		}
	} else {
		return Result{"", "", caseID}
	}
}

func toURL(center string, two_digit_yr int, day int, code int, case_serial_numbers int, format int) url.Values {
	if format == center_year_day_code_serial {
		res := url.Values{"appReceiptNum": {fmt.Sprintf("%s%d%03d%d%04d", center, two_digit_yr, day, code, case_serial_numbers)}}
		return res
	} else {
		res := url.Values{"appReceiptNum": {fmt.Sprintf("%s%d%d%03d%04d", center, two_digit_yr, code, day, case_serial_numbers)}}
		return res
	}
}

func clawAsync(center string, two_digit_yr int, day int, code int, case_serial_numbers int, format int, c chan Result) {
	c <- claw(center, two_digit_yr, day, code, case_serial_numbers, format)
}

func claw(center string, two_digit_yr int, day int, code int, case_serial_numbers int, format int) Result {
	url := toURL(center, two_digit_yr, day, code, case_serial_numbers, format)
	case_id := url.Get("appReceiptNum")
	res := get(url, 0)

	if res.Status != "" {
		case_status_store_mutex.Lock()
		ind, has := case_status_index_store[res]
		if !has {
			case_status_index_store[res] = case_status_index
			ind = case_status_index
			case_status_index++
		}
		case_status_store[case_id] = ind
		if len(case_status_store) > 0 && len(case_status_store)%int(report_freq) == 0 {
			now := time.Now().Unix()
			if now != last_record {
				fmt.Printf("\t\t\tQPS for previous %d: %d\n", report_freq, report_freq/(now-last_record))
				last_record = now
			}
		}
		case_status_store_mutex.Unlock()
	}

	return res
}

func getLastCaseNumber(center string, two_digit_yr int, day int, code int, format int) int {
	low := 1
	high := 1
	for (claw(center, two_digit_yr, day, code, high, format).Status != "" ||
		claw(center, two_digit_yr, day, code, high+1, format).Status != "" ||
		claw(center, two_digit_yr, day, code, high+2, format).Status != "" ||
		claw(center, two_digit_yr, day, code, high+3, format).Status != "" ||
		claw(center, two_digit_yr, day, code, high+4, format).Status != "") && high < 10000 {
		high *= 2
	}
	for low < high {
		mid := (low + high) / 2
		if claw(center, two_digit_yr, day, code, mid, format).Status != "" ||
			claw(center, two_digit_yr, day, code, mid+1, format).Status != "" ||
			claw(center, two_digit_yr, day, code, mid+2, format).Status != "" ||
			claw(center, two_digit_yr, day, code, mid+3, format).Status != "" ||
			claw(center, two_digit_yr, day, code, mid+4, format).Status != "" {
			low = mid + 1
		} else {
			high = mid
		}
	}
	return low - 1
}

func all(center string, two_digit_yr int, day int, code int, format int) {
	dir, _ := os.Getwd()
	var path string
	if format == center_year_day_code_serial {
		path = fmt.Sprintf("%s/data_center_year_day_code_serial_%d.json", dir, two_digit_yr)
	} else {
		path = fmt.Sprintf("%s/data_center_year_code_day_serial_%d.json", dir, two_digit_yr)
	}

	last := getLastCaseNumber(center, two_digit_yr, day, code, format)
	fmt.Printf("loading %s total of %d at day %d of format %d\n", center, last, day, format)
	c := make(chan Result)
	for i := 1; i < last; i++ {
		go clawAsync(center, two_digit_yr, day, code, i, format, c)
	}
	counter := make(map[string]map[int64]int)
	local_case_results := make([]Result, 0)
	for i := 1; i < last; i++ {
		cur := <-c
		if cur.Status == "" || cur.Form == "" {
			continue
		}

		local_case_results = append(local_case_results, cur)

		key := fmt.Sprintf("%s|%d|%d|%d|%s|%s", center, two_digit_yr, day, code, cur.Form, cur.Status)

		if counter[key] == nil {
			counter[key] = make(map[int64]int)
		}
		counter[key][epoch_day] += 1
	}

	fmt.Printf("counter: %+v\n", counter)

	mutex.Lock()
	existingCounter := make(map[string]map[int64]int)
	jsonFile, err := os.ReadFile(path)
	if err == nil {
		json.Unmarshal([]byte(jsonFile), &existingCounter)
	}
	getMerged(existingCounter, counter)
	b, _ := json.MarshalIndent(existingCounter, "", "  ")
	writeF(path, b)

	case_results = append(case_results, local_case_results...)

	mutex.Unlock()
	fmt.Printf("Done %s total of %d at day %d of format %d\n", center, last, day, format)
}

func readF(path string) []byte {
	f, err := os.ReadFile(path)
	for err != nil {
		fmt.Println("error read! " + err.Error() + "\n")
		f, err = os.ReadFile(path)
	}
	return f
}

func writeF(path string, content []byte) {
	err := os.WriteFile(path, content, 0666)
	for err != nil {
		fmt.Println("error write! " + err.Error() + "\n")
		err = os.WriteFile(path, content, 0666)
	}
}

func getMerged(m1, m2 map[string]map[int64]int) {
	for key, counter := range m2 {
		if m1[key] == nil {
			m1[key] = counter
		} else {
			for day, count := range counter {
				m1[key][day] = count
			}
		}
	}

	for _, counter := range m1 {
		for day := range counter {
			if epoch_day-day > 7 {
				delete(counter, day)
			}
		}
	}
}

func build_transitioning_map(delta int) {
	b_old, err1 := os.Open(fmt.Sprintf("./nocommit/%d.bytes", epoch_day-int64(delta)))
	b_new, err2 := os.Open(fmt.Sprintf("./nocommit/%d.bytes", epoch_day))
	if err1 != nil || err2 != nil {
		return
	}

	d_old := gob.NewDecoder(b_old)
	d_new := gob.NewDecoder(b_new)

	var raw_old RawStorage
	if err := d_old.Decode(&raw_old); err != nil {
		panic(err)
	}
	var raw_new RawStorage
	if err := d_new.Decode(&raw_new); err != nil {
		panic(err)
	}

	reverse_map_old := make(map[int]Result)
	for key, value := range raw_old.Index {
		reverse_map_old[value] = key
	}
	reverse_map_new := make(map[int]Result)
	for key, value := range raw_new.Index {
		reverse_map_new[value] = key
	}

	// center_year_day_code_serial|form|center|year|day|code|from|to -> count
	// center_year_code_day_serial|form|center|year|code|day|from|to -> count
	transitioning_map := make(map[string]int)
	for caseid, case_status_index_new := range raw_new.Status {
		case_status_new := reverse_map_new[case_status_index_new]
		case_status_old, ok := reverse_map_old[raw_old.Status[caseid]]
		if !ok {
			case_status_old = Result{"NEW_CASE", case_status_new.Form, case_status_new.CaseID}
		}
		if case_status_new != case_status_old {
			var center, year, day, code, serial, count_key string

			var case_form = case_status_old.Form
			if case_form == "NEW_CASE" {
				case_form = case_status_new.Form
			}
			if caseid[5:6] == "9" {
				fmt.Sscanf(caseid, "%3s%2s%1s%3s%4s", &center, &year, &code, &day, &serial)
				count_key = fmt.Sprintf("%s|%s|%s|%s|%s|%s|%s|%s", "center_year_code_day_serial", case_form, center, year, code, day, case_status_old.Status, case_status_new.Status)
			} else {
				fmt.Sscanf(caseid, "%3s%2s%3s%1s%4s", &center, &year, &day, &code, &serial)
				count_key = fmt.Sprintf("%s|%s|%s|%s|%s|%s|%s|%s", "center_year_day_code_serial", case_form, center, year, code, day, case_status_old.Status, case_status_new.Status)
			}

			if _, ok := transitioning_map[count_key]; !ok {
				transitioning_map[count_key] = 0
			}
			transitioning_map[count_key] = 1 + transitioning_map[count_key]
		}
	}
	dir, _ := os.Getwd()
	path := fmt.Sprintf("%s/transitioning_%d.json", dir, delta)
	existingTransitioningMap := make(map[int]map[string]int)
	jsonFile := readF(path)
	json.Unmarshal([]byte(jsonFile), &existingTransitioningMap)
	existingTransitioningMap[int(epoch_day)] = transitioning_map

	for day := range existingTransitioningMap {
		if int(epoch_day)-day > 7 {
			delete(existingTransitioningMap, day)
		}
	}

	b, _ := json.MarshalIndent(existingTransitioningMap, "", "  ")
	writeF(path, b)
}

func load_case_cache() {
	b, err := os.Open("./case_form_type_global_cache.bytes")
	if err != nil {
		return
	}
	d := gob.NewDecoder(b)
	if err := d.Decode(&case_form_type_global_cache); err != nil {
		panic(err)
	}
}

func persist_case_cache() {
	buffer := new(bytes.Buffer)
	e := gob.NewEncoder(buffer)
	err := e.Encode(case_form_type_global_cache)
	if err != nil {
		panic(err)
	}

	writeF("./case_form_type_global_cache.bytes", buffer.Bytes())
}

type Job struct {
	center       string
	two_digit_yr int
	day          int
	code         int
	format       int
}

var WorkerNum int = 1

func launchWorkers(queue chan *Job, wg *sync.WaitGroup) {
	for i := 0; i < WorkerNum; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range queue {
				all(job.center, job.two_digit_yr, job.day, job.code, job.format)
			}
		}()
	}
}

func main() {
	load_case_cache()

	queue := make(chan *Job, WorkerNum)
	wg := sync.WaitGroup{}

	launchWorkers(queue, &wg)

	for _, fy := range FYs {
		for _, name := range CENTER_NAMES {
			for day := 0; day <= 365; day++ {
				queue <- &Job{center: name, two_digit_yr: fy, day: day, code: 5, format: center_year_day_code_serial}
				queue <- &Job{center: name, two_digit_yr: fy, day: day, code: 9, format: center_year_code_day_serial}
			}
		}
	}

	close(queue)

	wg.Wait()

	fmt.Println("Job finished...")

	b, _ := json.MarshalIndent(case_results, "", "  ")
	writeF("case_results.json", b)

	buffer := new(bytes.Buffer)
	e := gob.NewEncoder(buffer)
	err := e.Encode(RawStorage{case_status_index_store, case_status_store})
	if err != nil {
		panic(err)
	}
	writeF(fmt.Sprintf("./nocommit/%d.bytes", epoch_day), buffer.Bytes())
	build_transitioning_map(1)
	build_transitioning_map(7)
	persist_case_cache()
}
