package gen3cwl

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	cwl "github.com/uc-cdis/cwl.go"
)

// this file contains all the code for managing the workflow graph
// i.e., assemble the graph, track dependencies
// recursively process workflows into *Tools
// dispatch *Tools to be executed by the K8sEngine
// NOTE: workflow steps are processed concurrently - see RunSteps()
// Task defines an instance of workflow/tool
// a task is a process is a node on the graph is one of [Workflow, CommandLineTool, ExpressionTool, ...]
type Task struct {
	// Engine          Engine
	Engine          *K8sEngine
	JobID           string
	Parameters      cwl.Parameters
	Root            *cwl.Root
	Outputs         cwl.Parameters
	Scatter         []string
	ScatterMethod   string
	ScatterTasks    map[int]*Task
	ScatterIndex    int // if a task gets scattered, each subtask belonging to that task gets enumerated, and that index is stored here
	Children        map[string]*Task
	unFinishedSteps map[string]struct{}
	outputIDMap     map[string]string
	originalStep    cwl.Step
}

func resolveGraph(rootMap map[string]*cwl.Root, curTask *Task) error {
	if curTask.Root.Class == "Workflow" {
		curTask.Children = make(map[string]*Task)
		for _, step := range curTask.Root.Steps {
			subworkflow, ok := rootMap[step.Run.Value]
			if !ok {
				panic(fmt.Sprintf("can't find workflow %v", step.Run.Value))
			}
			newTask := &Task{
				JobID:        curTask.JobID,
				Engine:       curTask.Engine,
				Root:         subworkflow,
				Parameters:   make(cwl.Parameters),
				originalStep: step,
			}
			resolveGraph(rootMap, newTask)
			// what to use as id? value or step.id
			curTask.Children[step.ID] = newTask
		}
	}
	return nil
}

// RunWorkflow parses a workflow and inputs and run it
func RunWorkflow(jobID string, workflow []byte, inputs []byte, engine *K8sEngine) error {
	var root cwl.Root
	err := json.Unmarshal(workflow, &root)
	if err != nil {
		return ParseError(err)
	}

	var originalParams cwl.Parameters
	err = json.Unmarshal(inputs, &originalParams)

	var params = make(cwl.Parameters)
	for id, value := range originalParams {
		params["#main/"+id] = value
	}
	if err != nil {
		return ParseError(err)
	}
	var mainTask *Task
	flatRoots := make(map[string]*cwl.Root)

	// iterate through master list of all process objects in packed cwl json
	for _, workflow := range root.Graphs {
		flatRoots[workflow.ID] = workflow // populate process.ID: process pair
		if workflow.ID == "#main" {
			mainTask = &Task{JobID: jobID, Root: workflow, Parameters: params, Engine: engine} // construct mainTask (task object for the top level workflow)
		}
	}
	if mainTask == nil {
		panic(fmt.Sprint("can't find main workflow"))
	}

	resolveGraph(flatRoots, mainTask)

	// mainTask.Run() // original, non-concurrent processing of workflow steps
	mainTask.GoRun() // runs workflow steps concurrently

	fmt.Print("\n\nFinished running workflow job.\n")
	fmt.Println("Here's the output:")
	PrintJSON(mainTask.Outputs)
	return nil
}

/*
concurrency notes:
1. Each step needs to wait until its input params are all populated before .Run()
2. mergeChildOutputs() needs to wait until the outputs are actually there to collect them - wait until the steps have finished running
*/

// GoRun for Concurrent Processing of workflow Steps
func (task *Task) GoRun() error {
	fmt.Printf("\nRunning task: %v\n", task.Root.ID)
	if task.Scatter != nil {
		task.runScatter()
		task.gatherScatterOutputs()
		return nil
	}

	if task.Root.Class == "Workflow" {
		// this process is a workflow, i.e., it has steps that must be run
		fmt.Printf("Handling workflow %v..\n", task.Root.ID)
		// concurrently run each of the workflow steps
		task.RunSteps()
		// merge outputs from all steps of this workflow to output for this workflow
		task.mergeChildOutputs()
	} else {
		// this process is not a workflow - it is a leaf in the graph (a *Tool) and gets dispatched to the task engine
		fmt.Printf("Dispatching task %v..\n", task.Root.ID)
		task.Engine.DispatchTask(task.JobID, task)
	}
	return nil
}

// for concurrent processing of steps of a workflow
// key point: the task does not get Run() until its input params are populated - that's how/where the dependencies get handled
func (task *Task) runStep(curStepID string, parentTask *Task) {
	fmt.Printf("\tProcessing Step: %v\n", curStepID)
	curStep := task.originalStep
	idMaps := make(map[string]string)
	for _, input := range curStep.In {
		taskInput := step2taskID(&curStep, input.ID)
		idMaps[input.ID] = taskInput // step input ID maps to [sub]task input ID

		// presently not handling the case of multiple sources for a given input parameter
		// see: https://www.commonwl.org/v1.0/Workflow.html#WorkflowStepInput
		// the section on "Merging", with the "MultipleInputFeatureRequirement" and "linkMerge" fields specifying either "merge_nested" or "merge_flattened"
		source := input.Source[0]

		// P: if source is an ID that points to an output in another step
		if depStepID, ok := parentTask.outputIDMap[source]; ok {
			// wait until dependency step output is there
			// and then assign output parameter of dependency step (which has just finished running) to input parameter of this step
			depTask := parentTask.Children[depStepID]
			outputID := depTask.Root.ID + strings.TrimPrefix(source, depStepID)
			for inputPresent := false; !inputPresent; _, inputPresent = task.Parameters[taskInput] {
				fmt.Println("\tWaiting for dependency task to finish running..")
				if len(depTask.Outputs) > 0 {
					fmt.Println("\tDependency task complete!")
					task.Parameters[taskInput] = depTask.Outputs[outputID]
					fmt.Println("\tSuccessfully collected output from dependency task.")
				}
				time.Sleep(2 * time.Second) // for testing..
			}
		} else if strings.HasPrefix(source, parentTask.Root.ID) {
			// if the input source to this step is not the outputID of another step
			// but is an input of the parent workflow (e.g. "#subworkflow_test.cwl/input_bam" in gen3_test.pack.cwl)
			// assign input parameter of parent workflow ot input parameter of this step
			task.Parameters[taskInput] = parentTask.Parameters[source]
		}
	}

	// reaching here implies one of <no step dependency> or <all step dependencies have been resolved/handled/run>

	if len(curStep.Scatter) > 0 {
		// subtask.Scatter = make([]string, len(curStep.Scatter))
		for _, i := range curStep.Scatter {
			task.Scatter = append(task.Scatter, idMaps[i])
		}
	}
	task.GoRun()
}

func (task *Task) RunSteps() {
	// store a map of {outputID: stepID} pairs to trace dependency
	task.setupOutputMap()
	// not sure if this will require a WaitGroup - seems to work fine without one
	for curStepID, subtask := range task.Children {
		go subtask.runStep(curStepID, task)
	}
}

func (task *Task) setupStepQueue() error {
	task.unFinishedSteps = make(map[string]struct{})
	for _, step := range task.Root.Steps {
		task.unFinishedSteps[step.ID] = struct{}{}
	}
	return nil
}

func (task *Task) getStep() string {
	for i := range task.unFinishedSteps {
		return i
	}
	return ""
}

// "#expressiontool_test.cwl" + "[#subworkflow_test.cwl]/test_expr/file_array"
// returns "#expressiontool_test.cwl/test_expr/file_array"
func step2taskID(step *cwl.Step, stepVarID string) string {
	return step.Run.Value + strings.TrimPrefix(stepVarID, step.ID)
}

// mergeChildOutputs maps outputs from children tasks to this task
// i.e., task.Outputs is a map of (outputID, outputValue) pairs
// for all the outputs of this workflow (this task is necessarily a workflow since only workflows have steps/children/subtasks)
func (task *Task) mergeChildOutputs() error {
	task.Outputs = make(cwl.Parameters)
	if task.Children == nil {
		panic(fmt.Sprintf("Can't call merge child outputs without childs %v \n", task.Root.ID))
	}
	for _, output := range task.Root.Outputs {
		if len(output.Source) == 1 {
			source := output.Source[0]
			stepID, ok := task.outputIDMap[source]
			if !ok {
				panic(fmt.Sprintf("Can't find output source %v", source))
			}
			subtaskOutputID := step2taskID(&task.Children[stepID].originalStep, source)
			for outputPresent := false; !outputPresent; _, outputPresent = task.Outputs[output.ID] {
				fmt.Printf("Waiting to merge child outputs for workflow %v ..\n", task.Root.ID)
				if outputVal, ok := task.Children[stepID].Outputs[subtaskOutputID]; ok {
					task.Outputs[output.ID] = outputVal
				}
				time.Sleep(time.Second * 2)
			}
		} else {
			panic(fmt.Sprintf("NOT SUPPORTED: don't know how to handle empty or array outputsource"))
		}
	}
	return nil
}

func (task *Task) setupOutputMap() error {
	task.outputIDMap = make(map[string]string)
	for _, step := range task.Root.Steps {
		for _, output := range step.Out {
			task.outputIDMap[output.ID] = step.ID
		}
	}
	return nil
}

///////////////////// Original Non-Concurrent Task.Run() method, for reference /////////////////////

// Run a task (original, non-concurrent method)
// a task is a process is a node on the graph
// a task can represent any of [Workflow, CommandLineTool, ExpressionTool, ...]
func (task *Task) Run() error {
	workflow := task.Root // use "process" instead of "workflow" as the variable name here
	params := task.Parameters

	fmt.Printf("\nRunning task: %v\n", workflow.ID)
	if task.Scatter != nil {
		task.runScatter()
		task.gatherScatterOutputs()
		return nil // stop processing scatter task
	}

	// if this process is a workflow
	// it is recursively resolved to a collection of *Tools
	// *Tools require no processing - they get dispatched to the task engine
	// *Tools are the leaves in the graph - the actual commands to be executed for the top-level workflow job
	if workflow.Class == "Workflow" {
		// create an unfinished steps map as a queue
		// a collection of the stepIDs for the steps of this workflow
		// stored in task.unFinishedSteps
		task.setupStepQueue()

		// store a map of {outputID: stepID} pairs to trace dependency
		task.setupOutputMap()

		var curStepID string
		var prevStepID string
		var curStep cwl.Step
		//  pick random step
		curStepID = task.getStep()

		/*
			Here is where the logic can shift
			For running steps concurrently
			Instead of a while loop
			The engine should just iterate through all the steps
			And run a goroutine for each step
			Dependencies will be handled by the logic:
			if dependent step(s), then wait until the dependent step(s) finish to collect input params
			only after all input params have been populated -> dispatch task
		*/
		// while there are unfinished steps
		for len(task.unFinishedSteps) > 0 {
			fmt.Printf("\tProcessing Step: %v\n", curStepID)
			prevStepID = ""

			subtask, ok := task.Children[curStepID] // retrieve task object for this step (subprocess) of the workflow
			if !ok {
				panic(fmt.Sprintf("can't find workflow %v", curStepID))
			}
			curStep = subtask.originalStep // info about this subprocess from the parent process' step list

			idMaps := make(map[string]string)
			for _, input := range curStep.In {
				subtaskInput := step2taskID(&curStep, input.ID)
				idMaps[input.ID] = subtaskInput // step input ID maps to [sub]task input ID
				for _, source := range input.Source {
					// P: if source is an ID that points to an output in another step
					if stepID, ok := task.outputIDMap[source]; ok {
						if _, ok := task.unFinishedSteps[stepID]; ok {
							prevStepID = stepID
							break
						} else {
							// assign output parameter of dependency step (which has already been executed) to input parameter of this step
							// HERE need to check engine stack to see if the dependency step has completed
							// how will this logic work - there's kind of a delay here, waiting for the dependency task to run
							// maybe a while-loop which loops until depTask has its output populated
							// but I don't want to block the rest of processing task.Run()
							// maybe there could be a go routine somewhere in here so non-dependent steps can run without waiting for each other
							depTask := task.Children[stepID]
							outputID := depTask.Root.ID + strings.TrimPrefix(source, stepID)
							inputPresent := false
							for ; !inputPresent; _, inputPresent = subtask.Parameters[subtaskInput] {
								fmt.Println("\tWaiting for dependency task to finish running..")
								if len(depTask.Outputs) > 0 {
									fmt.Println("\tDependency task complete!")
									subtask.Parameters[subtaskInput] = depTask.Outputs[outputID]
									fmt.Println("\tSuccessfully collected output from dependency task.")
								}
								time.Sleep(2 * time.Second) // for testing..
							}
						}
					} else if strings.HasPrefix(source, workflow.ID) {
						// if the input source to this step is not the outputID of another step
						// but is an input of the parent workflow (e.g. "#subworkflow_test.cwl/input_bam" in gen3_test.pack.cwl)
						// assign input parameter of parent workflow ot input parameter of this step

						// P: step.in.id is composed of {stepID}/{inputID}
						// P: it's mapped to the step's workflow's input definition
						// P: which has the structure of {stepWorkflowID}/{inputID}
						subtask.Parameters[subtaskInput] = params[source]
					}

				}
				if prevStepID != "" {
					// if we found a step dependency, then stop handling for this current step
					break
				}
			}

			// P: cancel processing this step, go to next loop to process dependent step
			if prevStepID != "" {
				curStepID = prevStepID
				fmt.Printf("\tUnresolved dependency! Going to dependency step: %v\n", curStepID)
				continue
			}

			// reaching here implies one of <no step dependency> or <all step dependencies have been resolved/handled/run>

			if len(curStep.Scatter) > 0 {
				// subtask.Scatter = make([]string, len(curStep.Scatter))
				for _, i := range curStep.Scatter {
					subtask.Scatter = append(subtask.Scatter, idMaps[i])
				}
			}
			subtask.Run()

			delete(task.unFinishedSteps, curStepID)
			// get random next step
			curStepID = task.getStep()
		}
		fmt.Println("\t\tMerging outputs for task ", task.Root.ID)
		// this mergeChildOutputs() function needs to wait until the child steps have finished running BEFORE attempting to merge outputs
		task.mergeChildOutputs() // for workflows only - merge outputs from all steps of this workflow to output for this workflow
	} else {
		// this process is not a workflow - it is a leaf in the graph (a *Tool) and gets dispatched to the task engine
		fmt.Printf("Dispatching task %v..\n", task.Root.ID)
		task.Engine.DispatchTask(task.JobID, task)
	}
	return nil
}
