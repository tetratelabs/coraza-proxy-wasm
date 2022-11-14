// Copyright The OWASP Coraza contributors
// SPDX-License-Identifier: Apache-2.0

//go:build mage
// +build mage

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/magefile/mage/mg"
	"github.com/magefile/mage/sh"
	"github.com/tetratelabs/wabin/binary"
	"github.com/tetratelabs/wabin/wasm"
)

var minGoVersion = "1.19"
var tinygoMinorVersion = "0.26"
var addLicenseVersion = "04bfe4ee9ca5764577b029acc6a1957fd1997153" // https://github.com/google/addlicense
var golangCILintVer = "v1.48.0"                                    // https://github.com/golangci/golangci-lint/releases
var gosImportsVer = "v0.3.1"                                       // https://github.com/rinchsan/gosimports/releases/tag/v0.3.1

var errCommitFormatting = errors.New("files not formatted, please commit formatting changes")
var errNoGitDir = errors.New("no .git directory found")

func init() {
	for _, check := range []func() error{
		checkTinygoVersion,
		checkGoVersion,
	} {
		if err := check(); err != nil {
			fmt.Printf("Error: %v\n", err)
			os.Exit(1)
		}
	}
}

// checkGoVersion checks the minium version of Go is supported.
func checkGoVersion() error {
	v, err := sh.Output("go", "version")
	if err != nil {
		return fmt.Errorf("unexpected go error: %v", err)
	}

	// Version can/cannot include patch version e.g.
	// - go version go1.19 darwin/arm64
	// - go version go1.19.2 darwin/amd64
	versionRegex := regexp.MustCompile("go([0-9]+).([0-9]+).?([0-9]+)?")
	compare := versionRegex.FindStringSubmatch(v)
	if len(compare) != 4 {
		return fmt.Errorf("unexpected go semver: %q", v)
	}
	compare = compare[1:]
	if compare[2] == "" {
		compare[2] = "0"
	}

	base := strings.SplitN(minGoVersion, ".", 3)
	if len(base) == 2 {
		base = append(base, "0")
	}
	for i := 0; i < 3; i++ {
		baseN, _ := strconv.Atoi(base[i])
		compareN, _ := strconv.Atoi(compare[i])
		if baseN > compareN {
			return fmt.Errorf("unexpected go version, minimum want %q, have %q", minGoVersion, strings.Join(compare, "."))
		}
	}
	return nil
}

// checkTinygoVersion checks that exactly the right tinygo version is supported because
// tinygo isn't stable yet.
func checkTinygoVersion() error {
	v, err := sh.Output("tinygo", "version")
	if err != nil {
		return fmt.Errorf("unexpected tinygo error: %v", err)
	}

	// Assume a dev build is valid.
	if strings.Contains(v, "-dev") {
		return nil
	}

	if !strings.HasPrefix(v, fmt.Sprintf("tinygo version %s", tinygoMinorVersion)) {
		return fmt.Errorf("unexpected tinygo version, wanted %s", tinygoMinorVersion)
	}

	return nil
}

// Format formats code in this repository.
func Format() error {
	if err := sh.RunV("go", "mod", "tidy"); err != nil {
		return err
	}
	// addlicense strangely logs skipped files to stderr despite not being erroneous, so use the long sh.Exec form to
	// discard stderr too.
	if _, err := sh.Exec(map[string]string{}, io.Discard, io.Discard, "go", "run", fmt.Sprintf("github.com/google/addlicense@%s", addLicenseVersion),
		"-c", "The OWASP Coraza contributors",
		"-s=only",
		"-y=",
		"-ignore", "**/*.yml",
		"-ignore", "**/*.yaml",
		"-ignore", "examples/**", "."); err != nil {
		return err
	}
	return sh.RunV("go", "run", fmt.Sprintf("github.com/rinchsan/gosimports/cmd/gosimports@%s", gosImportsVer),
		"-w",
		"-local",
		"github.com/corazawaf/coraza-proxy-wasm",
		".")
}

// Lint verifies code quality.
func Lint() error {
	if err := sh.RunV("go", "run", fmt.Sprintf("github.com/golangci/golangci-lint/cmd/golangci-lint@%s", golangCILintVer), "run"); err != nil {
		return err
	}

	mg.SerialDeps(Format)

	if sh.Run("git", "diff", "--exit-code") != nil {
		return errCommitFormatting
	}

	return nil
}

// Test runs all unit tests.
func Test() error {
	return sh.RunV("go", "test", "./...")
}

// Coverage runs tests with coverage and race detector enabled.
func Coverage() error {
	if err := os.MkdirAll("build", 0755); err != nil {
		return err
	}
	if err := sh.RunV("go", "test", "-race", "-coverprofile=build/coverage.txt", "-covermode=atomic", "-coverpkg=./...", "./..."); err != nil {
		return err
	}

	return sh.RunV("go", "tool", "cover", "-html=build/coverage.txt", "-o", "build/coverage.html")
}

// Doc runs godoc, access at http://localhost:6060
func Doc() error {
	return sh.RunV("go", "run", "golang.org/x/tools/cmd/godoc@latest", "-http=:6060")
}

// Check runs lint and tests.
func Check() {
	mg.SerialDeps(Lint, Test)
}

// Build builds the Coraza wasm plugin.
func Build() error {
	if err := os.MkdirAll("build", 0755); err != nil {
		return err
	}

	buildTags := []string{"custommalloc"}
	if os.Getenv("TIMING") == "true" {
		buildTags = append(buildTags, "timing", "proxywasm_timing")
	}
	if os.Getenv("MEMSTATS") == "true" {
		buildTags = append(buildTags, "memstats")
	}

	buildTagArg := fmt.Sprintf("-tags='%s'", strings.Join(buildTags, " "))

	// ~100MB initial heap
	initialPages := 2100
	if ipEnv := os.Getenv("INITIAL_PAGES"); ipEnv != "" {
		if ip, err := strconv.Atoi(ipEnv); err != nil {
			return err
		} else {
			initialPages = ip
		}
	}

	wd, err := os.Getwd()
	if err != nil {
		return err
	}

	script := fmt.Sprintf(`
cd /src && \
tinygo build -gc=none -opt=2 -o %s -scheduler=none -target=wasi %s`, filepath.Join("build", "mainraw.wasm"), buildTagArg)
	if err := sh.RunV("docker", "run", "--pull=always", "--rm", "-v", fmt.Sprintf("%s:/src", wd), "ghcr.io/corazawaf/coraza-proxy-wasm/buildtools-tinygo:main",
		"bash", "-c", script); err != nil {
		return err
	}

	return patchWasm(filepath.Join("build", "mainraw.wasm"), filepath.Join("build", "main.wasm"), initialPages)
}

// UpdateLibs updates the C++ filter dependencies.
func UpdateLibs() error {
	libs := []string{"aho-corasick", "libinjection", "mimalloc", "re2"}
	for _, lib := range libs {
		if err := sh.RunV("docker", "build", "-t", "ghcr.io/corazawaf/coraza-proxy-wasm/buildtools-"+lib, filepath.Join("buildtools", lib)); err != nil {
			return err
		}
		wd, err := os.Getwd()
		if err != nil {
			return err
		}
		if err := sh.RunV("docker", "run", "-it", "--rm", "-v", fmt.Sprintf("%s:/out", filepath.Join(wd, "lib")), "ghcr.io/corazawaf/coraza-proxy-wasm/buildtools-"+lib); err != nil {
			return err
		}
	}
	return nil
}

func E2e() error {
	mg.SerialDeps(E2eEnvoy, E2eIstio)
	return nil
}

// E2e runs e2e tests with a built plugin against the example deployment. Requires docker-compose.
func E2eEnvoy() error {
	return sh.RunV("docker-compose", "-f", "e2e/docker-compose.yml", "up", "--abort-on-container-exit", "tests")
}

func runK8sApply(file string, replacementKV ...string) error {
	fmt.Printf("Applying %q\n", file)
	if len(replacementKV) == 0 {
		return sh.RunV("kubectl", "apply", "-f", file)
	}

	if len(replacementKV)%2 != 0 {
		return errors.New("missing value for a replacement pair")
	}

	manifest, err := os.ReadFile(file)
	if err != nil {
		return err
	}

	patchedManifest := string(manifest)
	for i := 0; i < len(replacementKV)/2; i++ {
		patchedManifest = strings.Replace(patchedManifest, replacementKV[2*i], replacementKV[2*i+1], 1)
	}

	f, err := os.CreateTemp("", filepath.Base(file))
	if err != nil {
		return err
	}
	f.Write([]byte(patchedManifest))
	defer os.Remove(f.Name())

	if err := sh.RunV("kubectl", "apply", "-f", f.Name()); err != nil {
		return err
	}

	return nil
}

// References
// - https://kind.sigs.k8s.io/docs/user/loadbalancer/
func E2eIstio() error {
	const (
		clusterName = "coraza-proxy-wasm-e2e"
	)

	var (
		kind = "kind"
		//istioCTL = "/Users/jcchavezs/.getmesh/bin/getmesh istioctl"
		//kubeCTL     = "kubectl"
		dockerImage = fmt.Sprintf("corazawaf/coraza-proxy-wasm:%d", time.Now().Unix())
	)

	if clusters, err := sh.Output(kind, "get", "clusters"); err != nil {
		return err
	} else if !strings.Contains(clusters, clusterName) {
		err := sh.RunV(kind, "create", "cluster", "--name", clusterName, "--config", "./e2e/istio/cluster.yaml")
		if err != nil {
			return err
		}
	}
	//defer sh.RunV("kind", "delete", "cluster", "--name", clusterName)

	if err := sh.RunV("/Users/jcchavezs/.getmesh/bin/getmesh", "istioctl", "install", "--set", "profile=demo", "-y"); err != nil {
		return err
	}

	if err := sh.Run("kubectl", "label", "namespace", "default", "istio-injection=enabled", "--overwrite=true"); err != nil {
		return err
	}
	/*
		if err := runK8sApply("https://raw.githubusercontent.com/metallb/metallb/v0.13.7/config/manifests/metallb-native.yaml"); err != nil {
			return err
		}

		sh.RunV(kubeCTL, "wait", "--namespace", "metallb-system",
			"--for=condition=ready", "pod",
			"--selector=app=metallb",
			"--timeout=90s",
		)

		cidr, err := sh.Output("docker", "network", "inspect", "-f", "'{{.IPAM.Config}}'", "kind")
		if err != nil {
			return err
		}

		fmt.Println(cidr)

		if err := runK8sApply(
			"./e2e/istio/metallb-config.yaml",
			"${IP_START}", "172.19.255.200",
			"${IP_END}", "172.19.255.250",
		); err != nil {
			return err
		}
	*/

	if patch, err := os.ReadFile("./e2e/istio/patch-ingressgateway-nodeport.yaml"); err == nil {
		if err := sh.RunV("kubectl", "patch", "service", "istio-ingressgateway", "-n", "istio-system", "--patch", string(patch)); err != nil {
			return err
		}
	} else {
		return err
	}

	if err := sh.Run("docker", "build", "-t", dockerImage, "."); err != nil {
		return err
	}

	if err := sh.RunV(kind, "load", "docker-image", "kennethreitz/httpbin:latest", "--name", clusterName); err != nil {
		return err
	}

	if err := sh.RunV(kind, "load", "docker-image", dockerImage, "--name", clusterName); err != nil {
		return err
	}

	imageName, version, _ := strings.Cut(dockerImage, ":")
	fmt.Printf("Waiting for %q to be loaded in control plane\n", dockerImage)
	for {
		images, err := sh.Output("docker", "exec", "-it", clusterName+"-control-plane", "crictl", "images")
		if err != nil {
			return err
		}

		if strings.Contains(images, imageName) && strings.Contains(images, version) {
			break
		}
	}

	if err := runK8sApply("./e2e/istio/wasmplugin.yaml", "${IMAGE}", dockerImage); err != nil {
		return err
	}

	/*if err := runK8sApply("./e2e/istio/service.yaml"); err != nil {
		return err
	}

	ip, err := sh.Output(kubeCTL, "get", "svc/foo-service", "-o=jsonpath='{.status.loadBalancer.ingress[0].ip}'")
	if err != nil {
		return err
	}

	fmt.Println(ip)
	*/
	return nil
}

// Ftw runs ftw tests with a built plugin and Envoy. Requires docker-compose.
func Ftw() error {
	if err := sh.RunV("docker-compose", "--file", "ftw/docker-compose.yml", "build", "--pull"); err != nil {
		return err
	}
	defer func() {
		_ = sh.RunV("docker-compose", "--file", "ftw/docker-compose.yml", "down", "-v")
	}()
	env := map[string]string{
		"FTW_CLOUDMODE": os.Getenv("FTW_CLOUDMODE"),
	}
	if os.Getenv("ENVOY_NOWASM") == "true" {
		env["ENVOY_CONFIG"] = "/conf/envoy-config-nowasm.yaml"
	}
	task := "ftw"
	if os.Getenv("MEMSTATS") == "true" {
		task = "ftw-memstats"
	}
	return sh.RunWithV(env, "docker-compose", "--file", "ftw/docker-compose.yml", "run", "--rm", task)
}

// RunExample spins up the test environment, access at http://localhost:8080. Requires docker-compose.
func RunExample() error {
	return sh.RunV("docker-compose", "--file", "example/docker-compose.yml", "up", "-d", "envoy-logs")
}

// TeardownExample tears down the test environment. Requires docker-compose.
func TeardownExample() error {
	return sh.RunV("docker-compose", "--file", "example/docker-compose.yml", "down")
}

var Default = Build

func patchWasm(inPath, outPath string, initialPages int) error {
	raw, err := os.ReadFile(inPath)
	if err != nil {
		return err
	}
	mod, err := binary.DecodeModule(raw, wasm.CoreFeaturesV2)
	if err != nil {
		return err
	}

	mod.MemorySection.Min = uint32(initialPages)

	for _, imp := range mod.ImportSection {
		switch {
		case imp.Name == "fd_filestat_get":
			imp.Name = "fd_fdstat_get"
		case imp.Name == "path_filestat_get":
			imp.Module = "env"
			imp.Name = "proxy_get_header_map_value"
		}
	}

	out := binary.EncodeModule(mod)
	if err = os.WriteFile(outPath, out, 0644); err != nil {
		return err
	}

	return nil
}
