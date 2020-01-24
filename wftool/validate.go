package main

import "fmt"

// WorkflowJSON ..
type WorkflowJSON struct {
	Graph      *[]map[string]interface{} `json:"$graph"`
	CWLVersion string                    `json:"cwlVersion"`
}

// Grievances ..
type Grievances []string

// WorkflowGrievances ..
type WorkflowGrievances struct {
	Main      Grievances            `json:"main"`
	ByProcess map[string]Grievances `json:"byProcess"`
}

// Validator ..
type Validator struct {
	Workflow   *WorkflowJSON
	Grievances *WorkflowGrievances
}

func (g Grievances) log(f string, vs ...interface{}) {
	g = append(g, fmt.Sprintf(f, vs...))
}

// ValidateWorkflow ..
// this function feels exceedingly awkward
func ValidateWorkflow(wf *WorkflowJSON) (bool, *WorkflowGrievances) {
	v := &Validator{Workflow: wf}
	valid := v.Validate()
	return valid, v.Grievances
}

// Validate ..
func (v *Validator) Validate() bool {
	g := &WorkflowGrievances{
		Main:      make(Grievances, 0),
		ByProcess: make(map[string]Grievances),
	}
	v.Grievances = g

	// collect grievances

	// check if '$graph' field is populated
	if v.Workflow.Graph == nil {
		g.Main.log("missing graph")
	}

	// check version
	// here also validate that the cwlVersion matches
	// the version currently supported by mariner
	// todo
	if v.Workflow.CWLVersion == "" {
		g.Main.log("missing cwlVersion")
	}

	// check that '#main' routine (entrypoint into the graph) exists
	foundMain := false
	for _, obj := range *v.Workflow.Graph {
		if obj["id"] == "#main" {
			foundMain = true
			// recursively validate the whole graph
			fmt.Printf("found #main, validating workflow")
			v.validate(obj, "")
			break
		}
	}
	if !foundMain {
		g.Main.log("missing '#main' workflow")
	}

	// if there are grievances, report them
	if !g.empty() {
		return false
	}
	fmt.Println("validator says 'no grievances to report'")
	return true
}

func (wfg *WorkflowGrievances) empty() bool {
	if len(wfg.Main) > 0 {
		return false
	}
	for _, g := range wfg.ByProcess {
		if len(g) > 0 {
			return false
		}
	}
	return true
}

// check required field
func fieldCheck(obj map[string]interface{}, field string, g Grievances) bool {
	valid := true
	if i, ok := obj[field]; !ok {
		g.log("missing required field: '%v'", field)
		valid = false
	} else if mapToArray[field] {
		// enforce array structure
		// because of cwl.go's internals
		// later, if I change cwl.go library to be map-based instead of array-based
		// this check has to change to enforce map[string]interface{} structure
		if _, ok = i.([]interface{}); !ok {
			g.log("value for field '%v' must be an array", field)
			valid = false
		}
	}
	return valid
}

// recursively validate each cwl object in the graph
// log any grievances encountered
func (v *Validator) validate(obj map[string]interface{}, parentID string) {
	id := obj["id"].(string) // is this a safe assumption - no need to risk
	fmt.Println("validating obj ", id)
	g := make(Grievances, 0)
	v.Grievances.ByProcess[id] = g

	// collect grievances for this object

	// NOTE: don't need super specific checks
	// just rough checks are okay for first build

	// all general checks here
	var commonFields = []string{
		"inputs",
		"outputs",
		"class",
	}

	for _, field := range commonFields {
		fmt.Println("checking field ", field)
		fieldCheck(obj, field, g)
	}

	var class string
	class, ok := obj["class"].(string)
	if !ok {
		return
	}
	// here all class-specific checks
	switch class {
	case "CommandLineTool":
		fmt.Println("checking CLT")
		// no specific validation here yet
	case "Workflow":
		fmt.Println("checking WF steps")
		if valid := fieldCheck(obj, "steps", g); valid {
			steps := obj["steps"].([]interface{})
			for _, step := range steps {
				// calls validate(obj) on referenced cwl obj
				v.validateStep(step, id)
			}
		}
	case "ExpressionTool":
		fmt.Println("checking ET")
		fieldCheck(obj, "expression", g)
	default:
		g.log(fmt.Sprintf("invalid value for field 'class': %v", class))
	}
}

var stepFields = []string{
	// "id",
	"in",
	"out",
	"run",
}

// validate a workflow step
// call validate routine on referenced graph object
// NOTE: this is far from clean, but works
// REFACTOR
func (v *Validator) validateStep(i interface{}, parentID string) {
	g := v.Grievances.ByProcess[parentID]
	step, ok := i.(map[string]interface{})
	if !ok {
		g.log("step is not a map")
		return
	}
	i, ok = step["id"]
	if !ok {
		g.log("step missing id")
	}
	id, ok := i.(string)
	if !ok {
		g.log("invalid type for id field")
	}
	fmt.Println("validating step ", id)
	for _, field := range stepFields {
		_, ok = step[field]
		if !ok {
			g.log("step '%v' missing field: %v", id, field)
		}
	}
	i, ok = step["run"]
	if !ok {
		return
	}
	run, ok := i.(string)
	if !ok {
		g.log("step '%v' invalid type for field: %v", id, "run")
		return
	}

	// could write small fn to retrieve string val from map[string]interface{} obj

	var refObj map[string]interface{}
	for _, obj := range *v.Workflow.Graph {
		if obj["id"].(string) == run {
			fmt.Println("found ref obj with id ", run)
			refObj = obj
		}
	}
	if refObj == nil {
		fmt.Println("failed to find ref obj with id ", run)
		g.log("for step '%v' failed to find referenced cwl obj: %v", id, run)
		return
	}
	v.validate(refObj, parentID)
}
