// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package schedulerutils

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/microsoft/CBL-Mariner/toolkit/tools/internal/file"
	"github.com/microsoft/CBL-Mariner/toolkit/tools/internal/logger"
	"github.com/microsoft/CBL-Mariner/toolkit/tools/internal/pkggraph"
	"github.com/microsoft/CBL-Mariner/toolkit/tools/internal/pkgjson"
	"github.com/microsoft/CBL-Mariner/toolkit/tools/internal/retry"
	"github.com/microsoft/CBL-Mariner/toolkit/tools/internal/sliceutils"
	"github.com/microsoft/CBL-Mariner/toolkit/tools/scheduler/buildagents"
	"gonum.org/v1/gonum/graph"
	"gonum.org/v1/gonum/graph/traverse"
)

// BuildChannels represents the communicate channels used by a build agent.
type BuildChannels struct {
	Requests         <-chan *BuildRequest
	PriorityRequests <-chan *BuildRequest
	Results          chan<- *BuildResult
	Cancel           <-chan struct{}
	Done             <-chan struct{}
}

// BuildRequest represents the results of a build agent trying to build a given node.
type BuildRequest struct {
	Node           *pkggraph.PkgNode
	PkgGraph       *pkggraph.PkgGraph
	AncillaryNodes []*pkggraph.PkgNode
	CanUseCache    bool
	IsDelta        bool
}

// BuildResult represents the results of a build agent trying to build a given node.
type BuildResult struct {
	AncillaryNodes []*pkggraph.PkgNode
	BuiltFiles     []string
	Err            error
	LogFile        string
	Node           *pkggraph.PkgNode
	Ignored        bool
	UsedCache      bool
	WasDelta       bool
}

// selectNextBuildRequest selects a job based on priority:
//  1. Bail out if the jobs are cancelled
//  2. There is something in the priority queue
//  3. Any job in either normal OR priority queue
//     OR are the jobs done/cancelled
func selectNextBuildRequest(channels *BuildChannels) (req *BuildRequest, finish bool) {
	select {
	case <-channels.Cancel:
		logger.Log.Warn("Cancellation signal received")
		return nil, true
	default:
		select {
		case req = <-channels.PriorityRequests:
			if req != nil {
				logger.Log.Tracef("PRIORITY REQUEST: %v", *req)
			}
			return req, false
		default:
			select {
			case req = <-channels.PriorityRequests:
				if req != nil {
					logger.Log.Tracef("PRIORITY REQUEST: %v", *req)
				}
				return req, false
			case req = <-channels.Requests:
				if req != nil {
					logger.Log.Tracef("normal REQUEST: %v", *req)
				}
				return req, false
			case <-channels.Cancel:
				logger.Log.Warn("Cancellation signal received")
				return nil, true
			case <-channels.Done:
				logger.Log.Debug("Worker finished signal received")
				return nil, true
			}
		}
	}
}

// BuildNodeWorker process all build requests, can be run concurrently with multiple instances.
func BuildNodeWorker(channels *BuildChannels, agent buildagents.BuildAgent, graphMutex *sync.RWMutex, buildAttempts int, checkAttempts int, ignoredPackages []*pkgjson.PackageVer) {
	// Track the time a worker spends waiting on a task. We will add a timing node each time we finish processing a request, and stop
	// it when we pick up the next request
	for req, cancelled := selectNextBuildRequest(channels); !cancelled && req != nil; req, cancelled = selectNextBuildRequest(channels) {
		res := &BuildResult{
			Node:           req.Node,
			AncillaryNodes: req.AncillaryNodes,
			WasDelta:       req.IsDelta,
		}

		switch req.Node.Type {
		case pkggraph.TypeLocalBuild:
			res.UsedCache, res.Ignored, res.BuiltFiles, res.LogFile, res.Err = buildBuildNode(req.Node, req.PkgGraph, graphMutex, agent, req.CanUseCache, buildAttempts, ignoredPackages)
			if res.Err == nil {
				setAncillaryBuildNodesStatus(req, pkggraph.StateUpToDate)
			} else {
				setAncillaryBuildNodesStatus(req, pkggraph.StateBuildError)
			}

		case pkggraph.TypeTest:
			res.Ignored, res.LogFile, res.Err = buildTestNode(req.Node, req.PkgGraph, graphMutex, agent, req.CanUseCache, checkAttempts, ignoredPackages)
			if res.Err == nil {
				setAncillaryBuildNodesStatus(req, pkggraph.StateUpToDate)
			} else {
				setAncillaryBuildNodesStatus(req, pkggraph.StateBuildError)
			}

		case pkggraph.TypeLocalRun, pkggraph.TypeGoal, pkggraph.TypeRemoteRun, pkggraph.TypePureMeta, pkggraph.TypePreBuilt:
			res.UsedCache = req.CanUseCache

		case pkggraph.TypeUnknown:
			fallthrough

		default:
			res.Err = fmt.Errorf("invalid node type %v on node %v", req.Node.Type, req.Node)
		}

		channels.Results <- res
		// Track the time a worker spends waiting on a task
	}
	logger.Log.Debug("Worker done")
}

// buildBuildNode builds a TypeLocalBuild node, either used a cached copy if possible or building the corresponding SRPM.
func buildBuildNode(node *pkggraph.PkgNode, pkgGraph *pkggraph.PkgGraph, graphMutex *sync.RWMutex, agent buildagents.BuildAgent, canUseCache bool, buildAttempts int, ignoredPackages []*pkgjson.PackageVer) (usedCache, ignored bool, builtFiles []string, logFile string, err error) {
	var missingFiles []string

	baseSrpmName := node.SRPMFileName()
	usedCache, builtFiles, missingFiles = pkggraph.IsSRPMPrebuilt(node.SrpmPath, pkgGraph, graphMutex)
	ignored = sliceutils.Contains(ignoredPackages, node.VersionedPkg, sliceutils.PackageVerMatch)

	if ignored {
		logger.Log.Debugf("%s explicitly marked to be ignored.", baseSrpmName)
		return
	}

	if canUseCache && usedCache {
		logger.Log.Debugf("%s is prebuilt, skipping", baseSrpmName)
		return
	}

	// Print a message if a package is partially built but needs to be regenerated because its missing something.
	if len(missingFiles) > 0 && len(builtFiles) > 0 {
		logger.Log.Infof("SRPM '%s' is being rebuilt due to partially missing components: %v", node.SrpmPath, missingFiles)
	}

	usedCache = false

	dependencies := getBuildDependencies(node, pkgGraph, graphMutex)

	logger.Log.Infof("Building: %s", baseSrpmName)
	builtFiles, logFile, err = buildSRPMFile(agent, buildAttempts, node.SrpmPath, node.Architecture, dependencies)
	return
}

// buildTestNode tests a TypeTest node.
func buildTestNode(node *pkggraph.PkgNode, pkgGraph *pkggraph.PkgGraph, graphMutex *sync.RWMutex, agent buildagents.BuildAgent, canUseCache bool, checkAttempts int, ignoredPackages []*pkgjson.PackageVer) (ignored bool, logFile string, err error) {
	baseSrpmName := node.SRPMFileName()
	ignored = sliceutils.Contains(ignoredPackages, node.VersionedPkg, sliceutils.PackageVerMatch)

	if ignored {
		logger.Log.Debugf("%s (test) explicitly marked to be ignored.", baseSrpmName)
		return
	}

	if canUseCache {
		logger.Log.Debugf("All dependencies for '%s' were prebuilt, skipping its test.", baseSrpmName)
		return
	}

	dependencies := getBuildDependencies(node, pkgGraph, graphMutex)

	logger.Log.Infof("Testing: %s", baseSrpmName)
	logFile, err = testSRPMFile(agent, checkAttempts, node.SrpmPath, node.Architecture, dependencies)
	return
}

// getBuildDependencies returns a list of all dependencies that need to be installed before the node can be built.
func getBuildDependencies(node *pkggraph.PkgNode, pkgGraph *pkggraph.PkgGraph, graphMutex *sync.RWMutex) (dependencies []string) {
	graphMutex.RLock()
	defer graphMutex.RUnlock()

	// Use a map to avoid duplicate entries
	dependencyLookup := make(map[string]bool)

	search := traverse.BreadthFirst{}

	// Skip traversing any build nodes to avoid other package's buildrequires.
	search.Traverse = func(e graph.Edge) bool {
		toNode := e.To().(*pkggraph.PkgNode)
		return toNode.Type != pkggraph.TypeLocalBuild
	}

	search.Walk(pkgGraph, node, func(n graph.Node, d int) (stopSearch bool) {
		dependencyNode := n.(*pkggraph.PkgNode)

		rpmPath := dependencyNode.RpmPath
		if rpmPath == "" || rpmPath == pkggraph.NoRPMPath || rpmPath == node.RpmPath {
			return
		}

		dependencyLookup[rpmPath] = true

		return
	})

	dependencies = sliceutils.SetToSlice(dependencyLookup)

	return
}

// parseCheckSection reads the package build log file to determine if the %check section passed or not
func parseCheckSection(logFile string) (err error) {
	logFileObject, err := os.Open(logFile)
	// If we can't open the log file, that's a build error.
	if err != nil {
		logger.Log.Errorf("Failed to open log file '%s' while checking package test results. Error: %v", logFile, err)
		return
	}
	defer logFileObject.Close()
	for scanner := bufio.NewScanner(logFileObject); scanner.Scan(); {
		currLine := scanner.Text()
		// Anything besides 0 is a failed test
		if strings.Contains(currLine, "CHECK DONE") {
			if strings.Contains(currLine, "EXIT STATUS 0") {
				return
			}
			failedLogFile := strings.TrimSuffix(logFile, ".test.log")
			failedLogFile = fmt.Sprintf("%s-FAILED_TEST-%d.log", failedLogFile, time.Now().UnixMilli())
			err = file.Copy(logFile, failedLogFile)
			if err != nil {
				logger.Log.Errorf("Log file copy failed. Error: %v", err)
				return
			}
			err = fmt.Errorf("package test failed. Test status line: %s", currLine)
			return
		}
	}
	return
}

// buildSRPMFile sends an SRPM to a build agent to build.
func buildSRPMFile(agent buildagents.BuildAgent, buildAttempts int, srpmFile, outArch string, dependencies []string) (builtFiles []string, logFile string, err error) {
	const (
		retryDuration = time.Second
		runCheck      = false
	)

	logBaseName := filepath.Base(srpmFile) + ".log"

	err = retry.Run(func() (buildErr error) {
		builtFiles, logFile, buildErr = agent.BuildPackage(srpmFile, logBaseName, outArch, runCheck, dependencies)
		return
	}, buildAttempts, retryDuration)

	return
}

// testSRPMFile sends an SRPM to a build agent to test.
func testSRPMFile(agent buildagents.BuildAgent, checkAttempts int, srpmFile string, outArch string, dependencies []string) (logFile string, err error) {
	const (
		retryDuration = time.Second
		runCheck      = true
	)

	// checkFailed is a flag to see if a non-null buildErr is from the %check section
	checkFailed := false
	logBaseName := filepath.Base(srpmFile) + ".test.log"

	err = retry.Run(func() (buildErr error) {
		_, logFile, buildErr = agent.BuildPackage(srpmFile, logBaseName, outArch, runCheck, dependencies)
		if buildErr != nil {
			logger.Log.Warnf("Test build for '%s' failed on a non-test build issue. Error: %s", srpmFile, err)
			return
		}

		buildErr = parseCheckSection(logFile)
		checkFailed = (buildErr != nil)
		return
	}, checkAttempts, retryDuration)

	if err != nil && checkFailed {
		logger.Log.Warnf("Tests failed for '%s'. Error: %s", srpmFile, err)
		err = nil
	}
	return
}

// setAncillaryBuildNodesStatus sets the NodeState for all of the request's ancillary nodes.
func setAncillaryBuildNodesStatus(req *BuildRequest, nodeState pkggraph.NodeState) {
	for _, node := range req.AncillaryNodes {
		if node.Type == pkggraph.TypeLocalBuild {
			node.State = nodeState
		}
	}
}
