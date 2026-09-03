//go:build cgo

// Command cuda-check is a minimal CUDA-driver probe: it dlopens libcuda.so.1
// (injected into the sandbox by the GKE GPU device plugin), calls cuInit(0) and
// cuDeviceGetCount(), and exits 0 only when at least one device initializes.
// sandbox-init runs it as the "CUDA works" half of its readiness probe. No CUDA
// toolkit or headers are required — the entry points are declared inline.
package main

/*
#cgo LDFLAGS: -ldl
#include <stdlib.h>
#include <dlfcn.h>

typedef int CUresult;

static void *cc_handle;
static CUresult (*cc_cuInit)(unsigned int);
static CUresult (*cc_cuDeviceGetCount)(int *);

static const char *cc_load(void) {
    cc_handle = dlopen("libcuda.so.1", RTLD_NOW | RTLD_GLOBAL);
    if (cc_handle == NULL) {
        cc_handle = dlopen("libcuda.so", RTLD_NOW | RTLD_GLOBAL);
    }
    if (cc_handle == NULL) {
        return "dlopen libcuda.so.1 failed";
    }
    cc_cuInit = (CUresult (*)(unsigned int)) dlsym(cc_handle, "cuInit");
    cc_cuDeviceGetCount = (CUresult (*)(int *)) dlsym(cc_handle, "cuDeviceGetCount");
    if (cc_cuInit == NULL || cc_cuDeviceGetCount == NULL) {
        return "cuInit/cuDeviceGetCount missing from libcuda";
    }
    return NULL;
}

static int cc_init(void)         { return cc_cuInit(0); }
static int cc_count(int *n)       { return cc_cuDeviceGetCount(n); }
*/
import "C"

import (
	"fmt"
	"os"
)

func main() {
	if msg := C.cc_load(); msg != nil {
		fmt.Fprintln(os.Stderr, "cuda-check:", C.GoString(msg))
		os.Exit(1)
	}
	if rc := int(C.cc_init()); rc != 0 {
		fmt.Fprintf(os.Stderr, "cuda-check: cuInit failed (CUresult=%d)\n", rc)
		os.Exit(1)
	}
	var n C.int
	if rc := int(C.cc_count(&n)); rc != 0 {
		fmt.Fprintf(os.Stderr, "cuda-check: cuDeviceGetCount failed (CUresult=%d)\n", rc)
		os.Exit(1)
	}
	if int(n) < 1 {
		fmt.Fprintln(os.Stderr, "cuda-check: no CUDA devices visible")
		os.Exit(1)
	}
	fmt.Printf("cuda-check: ok (%d device)\n", int(n))
}
