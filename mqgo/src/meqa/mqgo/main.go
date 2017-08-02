package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"meqa/mqplan"
	"meqa/mqswag"
	"meqa/mqutil"
	"path/filepath"
)

const (
	meqaDataDir     = "meqa_data"
	swaggerJSONFile = "swagger.json"
	testPlanFile    = "testplan.yml"
)

func main() {
	mqutil.Logger = mqutil.NewStdLogger()

	meqaPath := flag.String("meqa", meqaDataDir, "the directory that holds the meqa data and swagger.json files")
	swaggerFile := flag.String("swagger", swaggerJSONFile, "the swagger.json file name or URL")
	testPlanFile := flag.String("testplan", testPlanFile, "the test plan file name")

	flag.Parse()
	swaggerJsonPath := filepath.Join(*meqaPath, *swaggerFile)
	testPlanPath := filepath.Join(*meqaPath, *testPlanFile)
	if _, err := os.Stat(swaggerJsonPath); os.IsNotExist(err) {
		mqutil.Logger.Printf("can't load swagger file at the following location %s", swaggerJsonPath)
		return
	}
	if _, err := os.Stat(testPlanPath); os.IsNotExist(err) {
		mqutil.Logger.Printf("can't load test plan file at the following location %s", testPlanPath)
		return
	}

	// Test loading swagger.json
	swagger, err := mqswag.CreateSwaggerFromURL(swaggerJsonPath)
	if err != nil {
		mqutil.Logger.Printf("Error: %s", err.Error())
	}
	for pathName, pathItem := range swagger.Paths.Paths {
		fmt.Printf("%v:%v\n", pathName, pathItem)
	}
	fmt.Printf("%v", swagger.Paths.Paths["/pet"].Post)

	mqswag.ObjDB.Init(swagger)

	// Test loading test plan
	err = mqplan.Current.InitFromFile(testPlanPath, &mqswag.ObjDB)
	if err != nil {
		mqutil.Logger.Printf("Error loading test plan: %s", err.Error())
	}

	fmt.Println("\n====== running get pet by status ======")
	result, err := mqplan.Current.Run("get pet by status", nil)
	resultJson, _ := json.Marshal(result)
	fmt.Printf("\nresult:\n%s", resultJson)
	fmt.Printf("\nerr:\n%v", err)

	fmt.Println("\n====== running create user manual ======")
	result, err = mqplan.Current.Run("create user auto", nil)
	resultJson, _ = json.Marshal(result)
	fmt.Printf("\nresult:\n%s", resultJson)

	fmt.Printf("\nerr:\n%v", err)
}
