// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// An image configuration validator

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/microsoft/azurelinux/toolkit/tools/imagegen/configuration"
	"github.com/microsoft/azurelinux/toolkit/tools/imagegen/installutils"
	"github.com/microsoft/azurelinux/toolkit/tools/internal/exe"
	"github.com/microsoft/azurelinux/toolkit/tools/internal/logger"
	"github.com/microsoft/azurelinux/toolkit/tools/internal/pkgjson"
	"github.com/microsoft/azurelinux/toolkit/tools/internal/timestamp"
	"github.com/microsoft/azurelinux/toolkit/tools/pkg/profile"

	"gopkg.in/alecthomas/kingpin.v2"
)

var (
	app = kingpin.New("imageconfigvalidator", "A tool for validating image configuration files")

	logFlags  = exe.SetupLogFlags(app)
	profFlags = exe.SetupProfileFlags(app)

	input       = exe.InputStringFlag(app, "Path to the image config file.")
	baseDirPath = exe.InputDirFlag(app, "Base directory for relative file paths from the config.")

	timestampFile = app.Flag("timestamp-file", "File that stores timestamps for this program.").String()
)

func main() {
	const returnCodeOnError = 1

	app.Version(exe.ToolkitVersion)
	kingpin.MustParse(app.Parse(os.Args[1:]))
	logger.InitBestEffort(logFlags)

	prof, err := profile.StartProfiling(profFlags)
	if err != nil {
		logger.Log.Warnf("Could not start profiling: %s", err)
	}
	defer prof.StopProfiler()

	timestamp.BeginTiming("config validator", *timestampFile)
	defer timestamp.CompleteTiming()

	inPath, err := filepath.Abs(*input)
	logger.PanicOnError(err, "Error when calculating input path")
	baseDir, err := filepath.Abs(*baseDirPath)
	logger.PanicOnError(err, "Error when calculating input directory")

	logger.Log.Infof("Reading configuration file (%s)", inPath)
	config, err := configuration.LoadWithAbsolutePaths(inPath, baseDir)
	if err != nil {
		logger.Log.Fatalf("Failed while loading image configuration '%s': %s", inPath, err)
	}
	// Basic validation will occur during load, but we can add additional checking here.
	err = ValidateConfiguration(config)
	if err != nil {
		// Log an error here as opposed to panicing to keep the output simple
		// and only contain the error with the config file.
		logger.Log.Fatalf("Invalid configuration '%s': %s", inPath, err)
	}

	return
}

// ValidateConfiguration will run sanity checks on a configuration structure
func ValidateConfiguration(config configuration.Config) (err error) {
	timestamp.StartEvent("validating config", nil)
	defer timestamp.StopEvent(nil)

	err = config.IsValid()
	if err != nil {
		return
	}

	err = validatePackages(config)
	if err != nil {
		return
	}

	err = validateKickStartInstall(config)
	return
}

func validateKickStartInstall(config configuration.Config) (err error) {
	timestamp.StartEvent("validate kickstart", nil)
	defer timestamp.StopEvent(nil)

	// If doing a kickstart-style installation, then the image config file
	// must not have any partitioning info because that will be provided
	// by the preinstall script

	for _, systemConfig := range config.SystemConfigs {
		if systemConfig.IsKickStartBoot {
			if len(config.Disks) > 0 || len(systemConfig.PartitionSettings) > 0 {
				return fmt.Errorf("partition should not be specified in image config file when performing kickstart installation")
			}
		}
	}

	return
}

func validatePackages(config configuration.Config) (err error) {
	timestamp.StartEvent("validate packages", nil)
	defer timestamp.StopEvent(nil)

	const (
		validateError     = "failed to validate package lists in config"
		kernelPkgName     = "kernel"
		dracutFipsPkgName = "dracut-fips"
		fipsKernelCmdLine = "fips=1"
		userAddPkgName    = "shadow-utils"
	)

	for _, systemConfig := range config.SystemConfigs {
		packageList, err := installutils.PackageNamesFromSingleSystemConfig(systemConfig)
		if err != nil {
			return fmt.Errorf("%s: %w", validateError, err)
		}
		foundSELinuxPackage := false
		foundDracutFipsPackage := false
		foundUserAddPackage := false
		kernelCmdLineString := systemConfig.KernelCommandLine.ExtraCommandLine
		selinuxPkgName := systemConfig.KernelCommandLine.SELinuxPolicy
		if selinuxPkgName == "" {
			selinuxPkgName = configuration.SELinuxPolicyDefault
		}

		for _, pkg := range packageList {
			// The installer tools have an undocumented feature which can support both "pkg-name" and "pkg-name=version" formats.
			// This is in use, so we need to handle pinned versions in this check. Technically, 'tdnf' also supports "pkg-name-version" format,
			// but it is not easily distinguishable from "long-package-name" format so it will not be supported here.
			pkgVer, err := pkgjson.PackageStringToPackageVer(pkg)
			if err != nil {
				return fmt.Errorf("%s: %w", validateError, err)
			}

			if pkgVer.Name == kernelPkgName {
				return fmt.Errorf("%s: kernel should not be included in a package list, add via config file's [KernelOptions] entry", validateError)
			}
			if pkgVer.Name == dracutFipsPkgName {
				foundDracutFipsPackage = true
			}
			if pkgVer.Name == selinuxPkgName {
				foundSELinuxPackage = true
			}
			if pkgVer.Name == userAddPkgName {
				foundUserAddPackage = true
			}
		}
		if strings.Contains(kernelCmdLineString, fipsKernelCmdLine) || systemConfig.KernelCommandLine.EnableFIPS {
			if !foundDracutFipsPackage {
				return fmt.Errorf("%s: 'fips=1' provided on kernel cmdline, but '%s' package is not included in the package lists", validateError, dracutFipsPkgName)
			}
		}
		if systemConfig.KernelCommandLine.SELinux != configuration.SELinuxOff {
			if !foundSELinuxPackage {
				return fmt.Errorf("%s: [SELinux] selected, but '%s' package is not included in the package lists", validateError, selinuxPkgName)
			}
		}
		if len(systemConfig.Users) > 0 || len(systemConfig.Groups) > 0 {
			if !foundUserAddPackage {
				return fmt.Errorf("%s: the '%s' package must be included in the package lists when the image is configured to add users or groups", validateError, userAddPkgName)
			}
		}
	}

	return
}
