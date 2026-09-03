// Command print emits the comma-joined includedFiles glob list for a component,
// for use as the --included-files value of a Cloud Build PR trigger:
//
//	gcloud builds triggers create github --included-files="$(go run ./deploy/cloudbuild/prpaths/print controller)" ...
package main

import (
	"fmt"
	"os"

	"ignition.dev/ignition/deploy/cloudbuild/prpaths"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: print <api|controller|gateway>")
		os.Exit(2)
	}
	out := prpaths.IncludedFiles(os.Args[1])
	if out == "" {
		fmt.Fprintf(os.Stderr, "unknown component %q\n", os.Args[1])
		os.Exit(1)
	}
	fmt.Println(out)
}
