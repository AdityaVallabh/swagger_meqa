package mqplan

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io/ioutil"
	"math"
	"math/rand"
	"strings"
	"time"

	"gopkg.in/resty.v0"
	"gopkg.in/yaml.v2"

	"meqa/mqswag"
	"meqa/mqutil"

	"github.com/go-openapi/spec"
	"github.com/lucasjones/reggen"
	"github.com/xeipuuv/gojsonschema"
)

// Test represents a test object in the DSL
type Test struct {
	Name       string
	Path       string
	Method     string
	Ref        string
	Parameters map[string]interface{}
}

// Run runs the test. It only returns error when there is an internal error.
// Test case failures are not counted.
func (t *Test) Run(swagger *mqswag.Swagger, db mqswag.DB, plan *TestPlan) error {
	if len(t.Ref) != 0 {
		return plan.Run(t.Ref, swagger, db)
	}
	err := t.ResolveParameters(swagger, db, plan)
	if err != nil {
		return err
	}

	// TODO add check for http/https (operation schemes) and pointers
	switch t.Method {
	case resty.MethodGet:
		resp, err := resty.R().SetQueryParams(t.Parameters).Get(swagger.BasePath + "/" + t.Path)
		// TODO properly process resp. Check against the current DB to see if they match
		mqutil.Logger.Print(resp)

		return err
	default:
		str := fmt.Sprintf("Unknow method in test %s: %v", t.Name, t.Method)
		return errors.New(str)
	}
}

func getOperationByMethod(item *spec.PathItem, method string) *spec.Operation {
	switch method {
	case resty.MethodGet:
		return item.Get
	case resty.MethodPost:
		return item.Post
	case resty.MethodPut:
		return item.Put
	case resty.MethodDelete:
		return item.Delete
	case resty.MethodPatch:
		return item.Patch
	case resty.MethodHead:
		return item.Head
	case resty.MethodOptions:
		return item.Options
	}
	return nil
}

// Generate paramter value based on the spec.
func generateParameter(paramSpec *spec.Parameter, db mqswag.DB) (interface{}, error) {
	if paramSpec.Schema != nil {
		return generateBySchema(paramSpec.Schema, db)
	}
	if len(paramSpec.Enum) != 0 {
		return generateByEnum(paramSpec)
	}
	if len(paramSpec.Type) == 0 {
		return "", mqutil.NewError(mqutil.ErrInvalid, "Parameter doesn't have type")
	}
	if paramSpec.Type == gojsonschema.TYPE_OBJECT {
		return generateObject(paramSpec)
	}

	return generateByType(&paramSpec.SimpleSchema, &paramSpec.CommonValidations, paramSpec.Name+"-")
}

func generateByType(s *spec.SimpleSchema, v *spec.CommonValidations, prefix string) (interface{}, error) {
	switch s.Type {
	case gojsonschema.TYPE_ARRAY:
		return generateArray(s, v, prefix)
	case gojsonschema.TYPE_BOOLEAN:
		return generateBool(v)
	case gojsonschema.TYPE_INTEGER:
		return generateInt(v)
	case gojsonschema.TYPE_NUMBER:
		return generateFloat(v)
	case gojsonschema.TYPE_STRING:
		return generateString(s, v, prefix)
	}

	panic("not implemented")
}

// RandomTime generate a random time in the range of [t - r, t).
func RandomTime(t time.Time, r time.Duration) time.Time {
	return t.Add(-time.Duration(float64(r) * rand.Float64()))
}

// TODO we need to make it context aware. Based on different contexts we should generate different
// date ranges. Prefix is a prefix to use when generating strings. It's only used when there is
// no specified pattern in the swagger.json
func generateString(s *spec.SimpleSchema, v *spec.CommonValidations, prefix string) (string, error) {
	if s.Format == "date-time" {
		t := RandomTime(time.Now(), time.Hour*24*30)
		return t.Format(time.RFC3339), nil
	}
	if s.Format == "date" {
		t := RandomTime(time.Now(), time.Hour*24*30)
		return t.Format("2006-01-02"), nil
	}

	// If no pattern is specified, we use the field name + some numbers as pattern
	var pattern string
	length := 0
	if len(v.Pattern) != 0 {
		pattern = v.Pattern
		length = len(v.Pattern) * 2
	} else {
		pattern = prefix + "\\d+"
		length = len(prefix) + 5
	}
	g, err := reggen.NewGenerator(pattern)
	if err != nil {
		return "", mqutil.NewError(mqutil.ErrInvalid, err.Error())
	}
	str := g.Generate(length)

	if len(s.Format) == 0 || s.Format == "password" {
		return str, nil
	}
	if s.Format == "byte" {
		return base64.StdEncoding.EncodeToString([]byte(str)), nil
	}
	if s.Format == "binary" {
		return hex.EncodeToString([]byte(str)), nil
	}
	return "", mqutil.NewError(mqutil.ErrInvalid, fmt.Sprintf("Invalid format string: %s", s.Format))
}

func generateBool(v *spec.CommonValidations) (interface{}, error) {
	return rand.Intn(2) == 0, nil
}

func generateFloat(v *spec.CommonValidations) (float64, error) {
	var realmin float64
	if v.Minimum != nil {
		realmin = *v.Minimum
		if v.ExclusiveMinimum {
			realmin += 0.01
		}
	}
	var realmax float64
	if v.Maximum != nil {
		realmax = *v.Maximum
		if v.ExclusiveMaximum {
			realmax -= 0.01
		}
	}
	if realmin >= realmax {
		if v.Minimum == nil && v.Maximum == nil {
			realmin = -1.0
			realmax = 1.0
		} else if v.Minimum != nil {
			realmax = realmin + math.Abs(realmin)
		} else if v.Maximum != nil {
			realmin = realmax - math.Abs(realmax)
		} else {
			// both are present but conflicting
			return 0, mqutil.NewError(mqutil.ErrInvalid, fmt.Sprintf("specified min value %v is bigger than max %v",
				*v.Minimum, *v.Maximum))
		}
	}
	return rand.Float64()*(realmax-realmin) + realmin, nil
}

func generateInt(v *spec.CommonValidations) (int64, error) {
	f, err := generateFloat(v)
	if err != nil {
		return 0, err
	}
	i := int64(f)
	if v.Minimum != nil && i <= int64(*v.Minimum) {
		i++
	}
	return i, nil
}

func generateArray(s *spec.SimpleSchema, v *spec.CommonValidations, prefix string) (interface{}, error) {
	var maxItems int
	if v.MaxItems != nil {
		maxItems = int(*v.MaxItems)
		if maxItems < 0 {
			maxItems = 0
		}
	}
	var minItems int
	if v.MinItems != nil {
		minItems = int(*v.MinItems)
		if minItems < 0 {
			minItems = 0
		}
	}
	count := 0
	if minItems == maxItems && minItems != 0 {
		count = minItems
	}
	maxDiff := maxItems - minItems
	if maxDiff < 0 {
		maxDiff = 1
	}
	numItems := rand.Intn(int(maxDiff)) + minItems

	var ar []interface{}
	for i := 0; i < numItems; i++ {
		entry, err := generateByType(&s.Items.SimpleSchema, &s.Items.CommonValidations, prefix+"-")
		if err != nil {
			return nil, err
		}
		ar = append(ar, entry)
	}
	return ar, nil
}

func generateBySchema(schema *spec.Schema, db mqswag.DB) (string, error) {
	panic("not implemented")
}

func generateByEnum(paramSpec *spec.Parameter) (string, error) {
	return fmt.Sprintf("%v", paramSpec.Enum[rand.Intn(len(paramSpec.Enum))]), nil
}

// ResolveParameters fullfills the parameters for the specified request using the in-mem DB.
// The resolved parameters will be added to test.Parameters map.
func (t *Test) ResolveParameters(swagger *mqswag.Swagger, db mqswag.DB, plan *TestPlan) error {
	pathItem := swagger.Paths.Paths[t.Path]
	op := getOperationByMethod(&pathItem, t.Method)
	if op == nil {
		return mqutil.NewError(mqutil.ErrNotFound, fmt.Sprintf("Path %s not found in swagger file", t.Path))
	}

	for _, params := range op.Parameters {
		// We don't override the existing parameters
		if _, ok := t.Parameters[params.Name]; ok {
			continue
		}
		p, err := generateParameter(&params, db)
		if err != nil {
			return err
		}
		t.Parameters[params.Name] = p
		return nil
	}
	return nil
}

type TestCase []*Test

// Represents all the test cases in the DSL.
type TestPlan struct {
	CaseMap  map[string](TestCase)
	CaseList [](TestCase)
}

// Add a new TestCase, returns whether the Case is successfully added.
func (plan *TestPlan) Add(name string, testCase TestCase) error {
	if _, exist := plan.CaseMap[name]; exist {
		str := fmt.Sprintf("Duplicate name %s found in test plan", name)
		mqutil.Logger.Println(str)
		return errors.New(str)
	}
	plan.CaseMap[name] = testCase
	plan.CaseList = append(plan.CaseList, testCase)
	return nil
}

func (plan *TestPlan) AddFromString(data string) error {
	var caseMap map[string]TestCase
	err := yaml.Unmarshal([]byte(data), &caseMap)
	if err != nil {
		mqutil.Logger.Printf("The following is not a valud TestCase:\n%s", data)
		return err
	}
	for testName, testCase := range caseMap {
		for _, t := range testCase {
			if len(t.Method) != 0 {
				t.Method = strings.ToUpper(t.Method)
			}
		}
		err = plan.Add(testName, testCase)
		if err != nil {
			return err
		}
	}
	return nil
}

func (plan *TestPlan) InitFromFile(path string) error {
	plan.CaseMap = make(map[string]TestCase)
	plan.CaseList = nil

	data, err := ioutil.ReadFile(path)
	if err != nil {
		mqutil.Logger.Printf("Can't open the following file: %s", path)
		mqutil.Logger.Println(err.Error())
		return err
	}
	chunks := strings.Split(string(data), "---")
	for _, chunk := range chunks {
		plan.AddFromString(chunk)
	}
	return nil
}

// Run a named TestCase in the test plan.
func (plan *TestPlan) Run(name string, swagger *mqswag.Swagger, db mqswag.DB) (err error) {
	tc, ok := plan.CaseMap[name]
	if !ok || len(tc) == 0 {
		str := fmt.Sprintf("The following test case is not found: %s", name)
		mqutil.Logger.Println(str)
		return errors.New(str)
	}

	for _, test := range tc {
		err = test.Run(swagger, db, plan)
		if err != nil {
			return err
		}

	}
	return nil
}

// The current global TestPlan
var Current TestPlan
