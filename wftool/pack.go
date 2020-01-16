package wftool

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v2"
)

// error handling, in general, needs attention

// PackCWL serializes a single cwl byte to json
func PackCWL(cwl []byte, id string, path string) {
	cwlObj := new(interface{})
	yaml.Unmarshal(cwl, cwlObj)
	*cwlObj = nuConvert(*cwlObj, primaryRoutine, id, false, path)
	printJSON(cwlObj)
}

// 'path' is relative to prevPath
// except in the case where prevPath is "", and path is absolute
// which is the first call to packCWLFile
//
// at first call
// first try absolute path
// if err, try relative path - path relative to working dir
// if err, fail out
//
// always only handle absolute paths - keep things simple
// assume prevPath is absolute
// and path is relative to prevPath
// construct absolute path of `path`
//
// so:
// 'path' is relative to 'prevPath'
// 'prevPath' is absolute
// 1. construct abs(path)
// 2. ..
func packCWLFile(path string, prevPath string) (err error) {
	// here get absolute path of 'path' before reading file
	if err = os.Chdir(filepath.Dir(prevPath)); err != nil {
		return err
	}
	if err = os.Chdir(filepath.Dir(path)); err != nil {
		return err
	}
	absPath, err := os.Getwd()
	if err != nil {
		return err
	}
	cwl, err := ioutil.ReadFile(absPath)
	if err != nil {
		return err
	}
	// copying cwltool's pack id scheme
	// not sure if it's actually good or not
	// but for now, doing this
	id := fmt.Sprintf("#%v", filepath.Base(absPath))
	// 'path' here is absolute - implies prevPath is absolute
	PackCWL(cwl, id, absPath)
	return nil
}

// PrintJSON pretty prints a struct as JSON
func printJSON(i interface{}) {
	var see []byte
	var err error
	see, err = json.MarshalIndent(i, "", "   ")
	if err != nil {
		fmt.Printf("error printing JSON: %v", err)
	}
	fmt.Println(string(see))
}
