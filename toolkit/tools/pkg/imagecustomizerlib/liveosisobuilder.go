// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package imagecustomizerlib

import (
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/microsoft/azurelinux/toolkit/tools/imagecustomizerapi"
	"github.com/microsoft/azurelinux/toolkit/tools/imagegen/configuration"
	"github.com/microsoft/azurelinux/toolkit/tools/imagegen/diskutils"
	"github.com/microsoft/azurelinux/toolkit/tools/internal/file"
	"github.com/microsoft/azurelinux/toolkit/tools/internal/logger"
	"github.com/microsoft/azurelinux/toolkit/tools/internal/safechroot"
	"github.com/microsoft/azurelinux/toolkit/tools/internal/safeloopback"
	"github.com/microsoft/azurelinux/toolkit/tools/internal/safemount"
	"github.com/microsoft/azurelinux/toolkit/tools/internal/shell"
	"github.com/microsoft/azurelinux/toolkit/tools/pkg/isomakerlib"
	"golang.org/x/sys/unix"
)

const (
	bootx64Binary         = "bootx64.efi"
	grubx64Binary         = "grubx64.efi"
	grubx64NoPrefixBinary = "grubx64-noprefix.efi"

	grubCfgDir                 = "/boot/grub2"
	isoGrubCfg                 = "grub.cfg"
	pxeGrubCfg                 = "grub-pxe.cfg"
	pxeKernelsArgs             = "ip=dhcp rd.live.azldownloader=enable"
	pxeImageBaseUrlPlaceHolder = "http://pxe-image-base-url-place-holder"

	searchCommandTemplate   = "search --label %s --set root"
	rootValueLiveOSTemplate = "live:LABEL=%s"
	rootValuePxeTemplate    = "live:%s"

	isoBootDir        = "boot"
	initrdImage       = "initrd.img"
	vmLinuzPrefix     = "vmlinuz-"
	isoInitrdPath     = "/boot/" + initrdImage
	isoKernelPath     = "/boot/vmlinuz"
	isoBootloadersDir = "/efi/boot"

	// kernel arguments template
	kernelArgsLiveOSTemplate = " rd.shell rd.live.image rd.live.dir=%s rd.live.squashimg=%s rd.live.overlay=1 rd.live.overlay.overlayfs rd.live.overlay.nouserconfirmprompt "

	liveOSDir   = "liveos"
	liveOSImage = "rootfs.img"

	// location on output iso where some of the input mic configuration will be
	// saved for future iso-to-iso customizations.
	savedConfigsDir = "azl-image-customizer"
	// file holding the iso kernel parameters from the input mic configuration
	// to be re-appended/merged with newer configures for future iso-to-iso
	// customizations.
	savedConfigsFileName = "saved-configs.yaml"

	dracutConfig = `add_dracutmodules+=" dmsquash-live livenet "
add_drivers+=" overlay "
hostonly="no"
`
	// the total size of a collection of files is multiplied by the
	// expansionSafetyFactor to estimate a disk size sufficient to hold those
	// files.
	expansionSafetyFactor = 1.5
)

type IsoWorkingDirs struct {
	// 'isoBuildDir' is where intermediate files will be placed during the
	// build.
	isoBuildDir string
	// 'isoArtifactsDir' is where extracted and generated files will be placed
	// during the build.
	isoArtifactsDir string
	// 'isomakerBuildDir' will be deleted/re-created by IsoMaker before it
	// proceeds. It needs to be different from `isoBuildDir`.
	isomakerBuildDir string
}

// `IsoArtifacts` holds the extracted/generated artifacts necessary to build
// a LiveOS ISO image.
type IsoArtifacts struct {
	kernelVersion        string
	dracutPackageInfo    *DracutPackageInformation
	bootx64EfiPath       string
	grubx64EfiPath       string
	isoGrubCfgPath       string
	pxeGrubCfgPath       string
	savedConfigsFilePath string
	vmlinuzPath          string
	initrdImagePath      string
	squashfsImagePath    string
	additionalFiles      map[string]string // local-build-path -> iso-media-path
}

type LiveOSIsoBuilder struct {
	workingDirs IsoWorkingDirs
	artifacts   IsoArtifacts
	cleanupDirs []string
}

func (b *LiveOSIsoBuilder) addCleanupDir(dirName string) {
	b.cleanupDirs = append(b.cleanupDirs, dirName)
}

func (b *LiveOSIsoBuilder) cleanUp() error {
	var err error
	for i := len(b.cleanupDirs) - 1; i >= 0; i-- {
		cleanupErr := os.RemoveAll(b.cleanupDirs[i])
		if cleanupErr != nil {
			if err != nil {
				err = fmt.Errorf("%w:\nfailed to remove (%s): %w", err, b.cleanupDirs[i], cleanupErr)
			} else {
				err = fmt.Errorf("failed to clean-up (%s): %w", b.cleanupDirs[i], cleanupErr)
			}
		}
	}
	return err
}

type isoImageNameInfo struct {
	tag            string
	releaseVersion string
	baseName       string
	name           string // derived from the other fields.
}

func getImageNameFromImageBaseName(isoOutputBaseName string) isoImageNameInfo {
	// isoMaker constructs the final image name as follows:
	// {isoOutputBaseName}{releaseVersion}{imageNameTag}.iso
	var info isoImageNameInfo
	info.baseName = isoOutputBaseName
	info.releaseVersion = ""
	info.tag = ""
	info.name = info.baseName + info.releaseVersion + info.tag + ".iso"
	return info
}

// populateWriteableRootfsDir
//
//	copies the contents of the rootfs partition unto the build machine.
//
// input:
//   - 'sourceDir'
//     path to full image mount root.
//   - 'writeableRootfsDir'
//     path to the folder where the contents of the rootfsDevice will be
//     copied to.
//
// output:
//   - writeableRootfsDir will hold the contents of sourceDir.
func (b *LiveOSIsoBuilder) populateWriteableRootfsDir(sourceDir, writeableRootfsDir string) error {

	logger.Log.Debugf("Creating writeable rootfs")

	err := os.MkdirAll(writeableRootfsDir, os.ModePerm)
	if err != nil {
		return fmt.Errorf("failed to create folder %s:\n%w", writeableRootfsDir, err)
	}

	err = copyPartitionFiles(sourceDir+"/.", writeableRootfsDir)
	if err != nil {
		return fmt.Errorf("failed to copy rootfs contents to a writeable folder (%s):\n%w", writeableRootfsDir, err)
	}

	return nil
}

// stageIsoMakerInitrdArtifacts
//
//	IsoMaker looks for the vmlinuz/bootloader files inside the initrd image
//	file under specific directory structure.
//	This function stages those artifacts and places them under the same
//	directory structure expected by IsoMaker.
//	Later,  we run 'dracut' which takes this directory structure and embeds
//	it into the initrd image.
//	Finaly, the IsoMaker will read the initrd image and find the artifacts
//	it needs to copy to the final iso media.
//	Something to consider in the future: change IsoMaker so that it can pick
//	those artifacts from the build machine directly.
//
// inputs:
//   - 'writeableRootfsDir':
//     path to an existing folder holding the contents of the rootfs.
//   - 'isoMakerArtifactsStagingDir'
//     path to a folder where the extracted artifacts will stored under.
//
// outputs:
//
//	the artifacts will be stored in 'isoMakerArtifactsStagingDir'.
func (b *LiveOSIsoBuilder) stageIsoMakerInitrdArtifacts(writeableRootfsDir, isoMakerArtifactsStagingDir string) error {

	logger.Log.Debugf("Staging isomaker artifacts into writeable image")

	targetBootloadersInChroot := filepath.Join(isoMakerArtifactsStagingDir, "/efi/EFI/BOOT")
	targetBootloadersDir := filepath.Join(writeableRootfsDir, targetBootloadersInChroot)

	err := os.MkdirAll(targetBootloadersDir, os.ModePerm)
	if err != nil {
		return fmt.Errorf("failed to create %s\n%w", targetBootloadersDir, err)
	}

	sourceBoot64EfiPath := b.artifacts.bootx64EfiPath
	targetBoot64EfiPath := filepath.Join(targetBootloadersDir, bootx64Binary)
	err = file.Copy(sourceBoot64EfiPath, targetBoot64EfiPath)
	if err != nil {
		return fmt.Errorf("failed to stage bootloader file (bootx64.efi):\n%w", err)
	}

	sourceGrub64EfiPath := b.artifacts.grubx64EfiPath
	targetGrub64EfiPath := filepath.Join(targetBootloadersDir, grubx64Binary)
	err = file.Copy(sourceGrub64EfiPath, targetGrub64EfiPath)
	if err != nil {
		return fmt.Errorf("failed to stage bootloader file (grubx64.efi):\n%w", err)
	}

	targetVmlinuzLocalDir := filepath.Join(writeableRootfsDir, isoMakerArtifactsStagingDir)

	sourceVmlinuzPath := b.artifacts.vmlinuzPath
	targetVmlinuzPath := filepath.Join(targetVmlinuzLocalDir, "vmlinuz")
	err = file.Copy(sourceVmlinuzPath, targetVmlinuzPath)
	if err != nil {
		return fmt.Errorf("failed to stage vmlinuz:\n%w", err)
	}

	return nil
}

// prepareRootfsForDracut
//
//	ensures two things:
//	- initrd image build time configuration is in place.
//	- rootfs (squashfs) image contents are compatible with our LiveOS initrd
//	  boot flow.
//	note that the same rootfs is used for both:
//	(1) creating the initrd image and
//	(2) creating the squashfs image.
//
// inputs:
//   - writeableRootfsDir:
//     root directory of existing rootfs content to modify.
//
// outputs:
// - all changes will be applied to the specified rootfs directory in the input.
func (b *LiveOSIsoBuilder) prepareRootfsForDracut(writeableRootfsDir string) error {

	logger.Log.Debugf("Preparing writeable image for dracut")

	fstabFile := filepath.Join(writeableRootfsDir, "/etc/fstab")
	logger.Log.Debugf("Deleting fstab from %s", fstabFile)
	err := os.Remove(fstabFile)
	if err != nil {
		return fmt.Errorf("failed to delete fstab:\n%w", err)
	}

	targetConfigFile := filepath.Join(writeableRootfsDir, "/etc/dracut.conf.d/20-live-cd.conf")
	err = file.Write(dracutConfig, targetConfigFile)
	if err != nil {
		return fmt.Errorf("failed to create %s:\n%w", targetConfigFile, err)
	}

	return nil
}

// updateSavedConfigs
//
//		This function merges:
//	 - a subset of the user current input configuration yaml.
//	 - a subset of the user saved (from previous runs) input configuration yaml.
//
//	 Depending on the configuration the behavior can be:
//	 - Concatenation:
//	   - like in the case of iso-specific kernel parameters.
//	 - Replacement:
//	   - like in the case of the pxeImageUrl
//
//	 In case of replacement, priority is given to the new configuration if
//	 present.
//
// inputs:
//   - savedConfigsFilePath:
//     full path to the yaml configuration file hold configuration from previous
//     runs.
//   - newKernelArgs:
//     kernel argument specified by the user in this run.
//   - newPxeIsoImageUrl:
//     PXE ISO image URL specified by the user in this run.
//   - newOSDracutVersion:
//     Dracut package version of the rootfs provided by the user.
//
// outputs:
// - returns a SavedConfigs objects with the new merged values.
func updateSavedConfigs(savedConfigsFilePath string, newKernelArgs imagecustomizerapi.KernelExtraArguments,
	newPxeIsoImageBaseUrl string, newPxeIsoImageFileUrl string, newDracutPackageInfo *DracutPackageInformation) (updatedSavedConfigs *SavedConfigs, err error) {
	updatedSavedConfigs = &SavedConfigs{}
	updatedSavedConfigs.Iso.KernelCommandLine.ExtraCommandLine = newKernelArgs
	updatedSavedConfigs.Pxe.IsoImageBaseUrl = newPxeIsoImageBaseUrl
	updatedSavedConfigs.Pxe.IsoImageFileUrl = newPxeIsoImageFileUrl
	updatedSavedConfigs.OS.DracutPackageInfo = newDracutPackageInfo

	savedConfigs, err := loadSavedConfigs(savedConfigsFilePath)
	if err != nil {
		return nil, fmt.Errorf("failed to load saved configurations (%s):\n%w", savedConfigsFilePath, err)
	}

	if savedConfigs != nil {
		// do we have kernel arguments from a previous run?
		if savedConfigs.Iso.KernelCommandLine.ExtraCommandLine != "" {
			// If yes, add them before the new kernel arguments.
			savedArgs := strings.TrimSpace(string(savedConfigs.Iso.KernelCommandLine.ExtraCommandLine))
			newArgs := strings.TrimSpace(string(newKernelArgs))
			updatedSavedConfigs.Iso.KernelCommandLine.ExtraCommandLine = imagecustomizerapi.KernelExtraArguments(savedArgs + " " + newArgs)
		}

		// if the PXE iso image url is not set, set it to the value from the previous run.
		if newPxeIsoImageBaseUrl == "" && savedConfigs.Pxe.IsoImageBaseUrl != "" {
			updatedSavedConfigs.Pxe.IsoImageBaseUrl = savedConfigs.Pxe.IsoImageBaseUrl
		}

		if newPxeIsoImageFileUrl == "" && savedConfigs.Pxe.IsoImageFileUrl != "" {
			updatedSavedConfigs.Pxe.IsoImageFileUrl = savedConfigs.Pxe.IsoImageFileUrl
		}

		// if IsoImageBaseUrl is being set in this run (i.e. newPxeIsoImageBaseUrl != ""),
		// then make sure IsoImageFileUrl is unset (since both fields must be mutually
		// exclusive) - and vice versa.
		if newPxeIsoImageBaseUrl != "" {
			updatedSavedConfigs.Pxe.IsoImageFileUrl = ""
		}

		if newPxeIsoImageFileUrl != "" {
			updatedSavedConfigs.Pxe.IsoImageBaseUrl = ""
		}

		// newOSDracutVersion can be nil if the input is an ISO and the
		// configuration does not specify OS changes.
		// In such cases, the rootfs is intentionally not expanded (to save
		// time), and Dracut package information will not be retrieved from
		// there. Instead, we use the saved configuration which already has the
		// the dracut version.
		if newDracutPackageInfo == nil {
			updatedSavedConfigs.OS.DracutPackageInfo = savedConfigs.OS.DracutPackageInfo
		}
	}

	err = updatedSavedConfigs.persistSavedConfigs(savedConfigsFilePath)
	if err != nil {
		return nil, fmt.Errorf("failed to save iso configs:\n%w", err)
	}

	return updatedSavedConfigs, nil
}

func (b *LiveOSIsoBuilder) updateGrubCfg(isoGrubCfgFileName string, pxeGrubCfgFileName string,
	savedConfigs *SavedConfigs, outputImageBase string) error {

	inputContentString, err := file.Read(isoGrubCfgFileName)
	if err != nil {
		return err
	}

	searchCommand := fmt.Sprintf(searchCommandTemplate, isomakerlib.DefaultVolumeId)
	inputContentString, err = replaceSearchCommandAll(inputContentString, searchCommand)
	if err != nil {
		return fmt.Errorf("failed to update the search command in the iso grub.cfg:\n%w", err)
	}

	grubMkconfigEnabled := isGrubMkconfigConfig(inputContentString)
	if !grubMkconfigEnabled {
		var oldLinuxPath string
		inputContentString, oldLinuxPath, err = setLinuxPath(inputContentString, isoKernelPath)
		if err != nil {
			return fmt.Errorf("failed to update the kernel file path in the iso grub.cfg:\n%w", err)
		}

		inputContentString, err = replaceToken(inputContentString, oldLinuxPath, isoKernelPath)
		if err != nil {
			return fmt.Errorf("failed to update all the kernel file path occurances in the iso grub.cfg:\n%w", err)
		}

		var oldInitrdPath string
		inputContentString, oldInitrdPath, err = setInitrdPath(inputContentString, isoInitrdPath)
		if err != nil {
			return fmt.Errorf("failed to update the initrd file path in the iso grub.cfg:\n%w", err)
		}

		inputContentString, err = replaceToken(inputContentString, oldInitrdPath, isoInitrdPath)
		if err != nil {
			return fmt.Errorf("failed to update all the initrd file path occurances in the iso grub.cfg:\n%w", err)
		}
	} else {
		inputContentString, _, err = setLinuxOrInitrdPathAll(inputContentString, linuxCommand, isoKernelPath, true /*allowMultiple*/)
		if err != nil {
			return fmt.Errorf("failed to update the kernel file path in the iso grub.cfg:\n%w", err)
		}

		inputContentString, _, err = setLinuxOrInitrdPathAll(inputContentString, initrdCommand, isoInitrdPath, true /*allowMultiple*/)
		if err != nil {
			return fmt.Errorf("failed to update the initrd file path in the iso grub.cfg:\n%w", err)
		}
	}

	rootValue := fmt.Sprintf(rootValueLiveOSTemplate, isomakerlib.DefaultVolumeId)
	inputContentString, _, err = replaceKernelCommandLineArgValueAll(inputContentString, "root", rootValue, true /*allowMultiple*/)
	if err != nil {
		return fmt.Errorf("failed to update the root kernel argument in the iso grub.cfg:\n%w", err)
	}

	inputContentString, err = updateSELinuxCommandLineHelperAll(inputContentString, imagecustomizerapi.SELinuxModeDisabled,
		true /*allowMultiple*/, false /*requireKernelOpts*/)
	if err != nil {
		return fmt.Errorf("failed to set SELinux mode:\n%w", err)
	}

	liveosKernelArgs := fmt.Sprintf(kernelArgsLiveOSTemplate, liveOSDir, liveOSImage)
	additionalKernelCommandline := liveosKernelArgs + " " + string(savedConfigs.Iso.KernelCommandLine.ExtraCommandLine)

	inputContentString, err = appendKernelCommandLineArgsAll(inputContentString, additionalKernelCommandline,
		true /*allowMultiple*/, false /*requireKernelOpts*/)
	if err != nil {
		return fmt.Errorf("failed to update the kernel arguments with the LiveOS configuration and user configuration in the iso grub.cfg:\n%w", err)
	}

	err = file.Write(inputContentString, isoGrubCfgFileName)
	if err != nil {
		return fmt.Errorf("failed to write %s:\n%w", isoGrubCfgFileName, err)
	}

	// Check if the dracut version in use meets our minimum requirements for
	// PXE support.
	err = verifyDracutPXESupport(savedConfigs.OS.DracutPackageInfo)
	if err != nil {
		// MIC does not provide a way for the user to explicitly indicate that a
		// PXE bootable ISO is desired. Instead, MIC always tries to create one.
		// In cases that the source image does not meet the minimum requirements
		// for the PXE bootable ISO, MIC just reports that information to the user
		// and does not terminate the ISO creation process. No error is reported
		// because MIC does not know if the user is interested only in the ISO image,
		// or also in the PXE artifacts.
		logger.Log.Infof("cannot generate grub.cfg for PXE booting.\n%v", err)
	} else {
		err = generatePxeGrubCfg(inputContentString, savedConfigs.Pxe.IsoImageBaseUrl, savedConfigs.Pxe.IsoImageFileUrl,
			outputImageBase, pxeGrubCfgFileName)
		if err != nil {
			return fmt.Errorf("failed to create grub configuration for PXE booting.\n%w", err)
		}
	}

	return nil
}

// generatePxeGrubCfg
//
// given the content of the iso grub.cfg, this function derives the PXE
// equivalent.
//
// inputs:
//   - inputContentString:
//     iso grub.cfg content.
//   - pxeIsoImageBaseUrl:
//     url to a folder containing the iso image to download at boot time.
//     The function will append the outputImageBase to the url to form the full
//     url to the image.
//     For example, if pxeIsoImageBaseUrl is set to "http://192.168.0.1/liveos",
//     the final url will be "http://192.168.0.1/liveos/<outputImageBase>".
//     This parameter cannot be set if pxeIsoImageFileUrl is also set.
//   - pxeIsoImageFileUrl:
//     url to the iso image to download at boot time.
//     This parameter cannot be set if pxeIsoImageBaseUrl is also set.
//   - outputImageBase:
//     the generated iso name. This value will be used only if the pxeIsoImageFileUrl
//     is empty.
//   - pxeGrubCfgFileName:
//     path of file to hold the PXE grub configuration.
//
// returns:
//   - error: nil if successful, otherwise an error object.
//
// generates:
//   - grub configuration file for PXE booting.
func generatePxeGrubCfg(inputContentString string, pxeIsoImageBaseUrl string, pxeIsoImageFileUrl string,
	outputImageBase string, pxeGrubCfgFileName string) error {
	if pxeIsoImageBaseUrl != "" && pxeIsoImageFileUrl != "" {
		return fmt.Errorf("cannot set both iso image base url and full image url at the same time.")
	}

	// remove 'search' commands from PXE grub.cfg because it is not needed.
	inputContentString, err := removeCommandAll(inputContentString, "search")
	if err != nil {
		return fmt.Errorf("failed to remove the 'search' commands from PXE grub.cfg:\n%w", err)
	}

	// If the specified URL is not a full path to an iso, append the generated
	// iso file name to it.
	if pxeIsoImageFileUrl == "" {
		pxeIsoImageFileUrl, err = url.JoinPath(pxeIsoImageBaseUrl, getImageNameFromImageBaseName(outputImageBase).name)
		if err != nil {
			return fmt.Errorf("failed to concatenate URL (%s) and (%s)\n%w", pxeIsoImageBaseUrl, outputImageBase, err)
		}
	}
	rootValue := fmt.Sprintf(rootValuePxeTemplate, pxeIsoImageFileUrl)
	inputContentString, _, err = replaceKernelCommandLineArgValueAll(inputContentString, "root", rootValue, true /*allowMultiple*/)
	if err != nil {
		return fmt.Errorf("failed to update the root kernel argument with the PXE iso image url in the PXE grub.cfg:\n%w", err)
	}

	inputContentString, err = appendKernelCommandLineArgsAll(inputContentString, pxeKernelsArgs,
		true /*allowMultiple*/, false /*requireKernelOpts*/)
	if err != nil {
		return fmt.Errorf("failed to append the kernel arguments (%s) in the PXE grub.cfg:\n%w", pxeKernelsArgs, err)
	}

	err = file.Write(inputContentString, pxeGrubCfgFileName)
	if err != nil {
		return fmt.Errorf("failed to write %s:\n%w", pxeGrubCfgFileName, err)
	}

	return nil
}

// containsGrubNoPrefix
//
// given a list of file path, this function returns true if one of the files
// is named grubx64-noprefix.efi; otherwise it returns false.
//
// inputs:
//   - filePaths:
//     A list of file paths.
//
// outputs:
//   - boolean
//     true if grubx64-noprefix.efi is one of the files.
//     false otherwise.
func containsGrubNoPrefix(filePaths []string) bool {
	for _, filePath := range filePaths {
		if filepath.Base(filePath) == grubx64NoPrefixBinary {
			return true
		}
	}
	return false
}

// extractBootDirFiles
//
// given a rootfs, this function:
// - extracts the files under the /boot folder
//
// inputs:
//   - writeableRootfsDir:
//     A writeable folder where the rootfs content is.
//
// outputs:
//   - copied files and the following are populated:
//     b.artifacts.bootx64EfiPath
//     b.artifacts.grubx64EfiPath
//     b.artifacts.vmlinuzPath
//     b.artifacts.additionalFiles
func (b *LiveOSIsoBuilder) extractBootDirFiles(writeableRootfsDir string) error {

	b.artifacts.additionalFiles = make(map[string]string)

	// the following files will be re-created - no need to copy them only to
	// have them overwritten.
	var exclusions []*regexp.Regexp
	//
	// We will generate a new initrd later. So, we do not copy the initrd.img
	// that comes in the input full disk image.
	//
	exclusions = append(exclusions, regexp.MustCompile(`/boot/initrd\.img.*`))
	exclusions = append(exclusions, regexp.MustCompile(`/boot/initramfs-.*\.img.*`))
	//
	// On full disk images (generated by Mariner toolkit), there are two
	// grub.cfg files:
	// - <boot partition>/boot/grub2/grub.cfg:
	//   - mounted at /boot/efi/boot/grub2/grub.cfg.
	//   - empty except for redirection to the other grub.cfg.
	// - <rootfs partition>/boot/grub2/grub.cfg:
	//   - mounted at /boot/grub2/grub.cfg
	//   - has the actual grub configuration.
	//
	// When creating an iso image out of a full disk image, we do not need the
	// redirection mechanism, and hence we can do with only the full grub.cfg.
	//
	// To avoid confusion, we do not copy the redirection grub.cfg to the iso
	// media.
	//
	exclusions = append(exclusions, regexp.MustCompile(`/boot/efi/boot/grub2/grub\.cfg`))

	bootFolderFilePaths, err := file.EnumerateDirFiles(filepath.Join(writeableRootfsDir, "/boot"))
	if err != nil {
		return fmt.Errorf("failed to scan /boot folder:\n%w", err)
	}

	usingGrubNoPrefix := containsGrubNoPrefix(bootFolderFilePaths)

	for _, sourcePath := range bootFolderFilePaths {

		excluded := false
		for _, exclusion := range exclusions {
			match := exclusion.FindStringIndex(sourcePath)
			if match != nil {
				excluded = true
				break
			}
		}
		if excluded {
			logger.Log.Debugf("Not copying %s. File is either unnecessary or will be re-generated.", sourcePath)
			continue
		}

		targetPath := strings.Replace(sourcePath, writeableRootfsDir, b.workingDirs.isoArtifactsDir, -1)
		targetFileName := filepath.Base(targetPath)

		scheduleAdditionalFile := true

		switch targetFileName {
		case bootx64Binary:
			b.artifacts.bootx64EfiPath = targetPath
			// isomaker will extract this from initrd and copy it to include it
			// in the iso media - so no need to schedule it as an additional
			// file.
			scheduleAdditionalFile = false
		case grubx64Binary, grubx64NoPrefixBinary:
			b.artifacts.grubx64EfiPath = targetPath
			// isomaker will extract this from initrd and copy it to include it
			// in the iso media - so no need to schedule it as an additional
			// file.
			scheduleAdditionalFile = false
		case isoGrubCfg:
			if usingGrubNoPrefix {
				// When using the grubx64-noprefix.efi, the 'prefix' grub
				// variable is set to an empty string. When 'prefix' is an
				// empty string, and grubx64-noprefix.efi is run from an iso
				// media, the bootloader defaults to looking for grub.cfg at
				// <boot-media>/EFI/BOOT/grub.cfg.
				// So, below, we ensure that grub.cfg file will be placed where
				// grubx64-nopreifx.efi will be looking for it.
				//
				// Note that this grub.cfg is the only file that needs to be
				// copied to that EFI/BOOT location. The rest of the files (like
				// grubenv, etc) can be left under /boot as usual. This is
				// because grub.cfg still defines 'bootprefix' to be /boot.
				// So, once grubx64.efi loads EFI/BOOT/grub.cfg, it will set
				// bootprefix to the usual location boot/grub2 and will proceed
				// as usual from there.
				targetPath = filepath.Join(b.workingDirs.isoArtifactsDir, "EFI/BOOT", isoGrubCfg)
			}
			b.artifacts.isoGrubCfgPath = targetPath
			// We will place the pxe grub config next to the iso grub config.
			b.artifacts.pxeGrubCfgPath = filepath.Join(filepath.Dir(b.artifacts.isoGrubCfgPath), pxeGrubCfg)
			// grub.cfg is passed as a parameter to isomaker.
			scheduleAdditionalFile = false
		}
		if strings.HasPrefix(targetFileName, vmLinuzPrefix) {
			targetPath = filepath.Join(filepath.Dir(targetPath), "vmlinuz")
			b.artifacts.vmlinuzPath = targetPath
			// isomaker will extract this from initrd and copy it to include it
			// in the iso media - so no need to schedule it as an additional
			// file.
			scheduleAdditionalFile = false
		}

		err = file.NewFileCopyBuilder(sourcePath, targetPath).
			SetNoDereference().
			Run()
		if err != nil {
			return fmt.Errorf("failed to extract files from under the boot folder:\n%w", err)
		}

		if scheduleAdditionalFile {
			b.artifacts.additionalFiles[targetPath] = strings.TrimPrefix(targetPath, b.workingDirs.isoArtifactsDir)
		}
	}

	if b.artifacts.bootx64EfiPath == "" {
		return fmt.Errorf("failed to find the boot efi file (%s):\n"+
			"this file is provided by the (shim) package",
			bootx64Binary)
	}

	if b.artifacts.grubx64EfiPath == "" {
		return fmt.Errorf("failed to find the grub efi file (%s or %s):\n"+
			"this file is provided by either the (grub2-efi-binary) or the (grub2-efi-binary-noprefix) package",
			grubx64Binary, grubx64NoPrefixBinary)
	}

	return nil
}

// findKernelVersion
//
// given a rootfs, this function extracts the kernel version.
//
// inputs:
//   - writeableRootfsDir:
//     A writeable folder where the rootfs content is.
//
// outputs:
//   - the following is populated:
//     b.artifacts.kernelVersion
func (b *LiveOSIsoBuilder) findKernelVersion(writeableRootfsDir string) error {
	const kernelModulesDir = "/usr/lib/modules"

	kernelParentPath := filepath.Join(writeableRootfsDir, kernelModulesDir)
	kernelDirs, err := os.ReadDir(kernelParentPath)
	if err != nil {
		return fmt.Errorf("failed to enumerate kernels under (%s):\n%w", kernelParentPath, err)
	}

	// Filter out directories that are empty.
	// Some versions of Azure Linux 2.0 don't cleanup properly when the kernel package is uninstalled.
	filteredKernelDirs := []fs.DirEntry(nil)
	for _, kernelDir := range kernelDirs {
		kernelPath := filepath.Join(kernelParentPath, kernelDir.Name())
		empty, err := file.IsDirEmpty(kernelPath)
		if err != nil {
			return err
		}

		if !empty {
			filteredKernelDirs = append(filteredKernelDirs, kernelDir)
		}
	}

	if len(filteredKernelDirs) == 0 {
		return fmt.Errorf("did not find any kernels installed under (%s)", kernelModulesDir)
	}
	if len(filteredKernelDirs) > 1 {
		return fmt.Errorf("unsupported scenario: found more than one kernel under (%s)", kernelModulesDir)
	}
	b.artifacts.kernelVersion = filteredKernelDirs[0].Name()
	logger.Log.Debugf("Found installed kernel version (%s)", b.artifacts.kernelVersion)
	return nil
}

// prepareLiveOSDir
//
//	given a rootfs, this function:
//	- extracts the kernel version, and the files under the boot folder.
//	- stages bootloaders and vmlinuz to a specific folder structure.
//	This folder structure is to be included later in the initrd image when
//	it gets generated. IsoMaker extracts those artifacts from the initrd
//	image file and uses them.
//	-prepares the rootfs to run dracut (dracut will generate the initrd later).
//	- creates the squashfs.
//
// inputs:
//   - 'inputSavedConfigsFilePath':
//   - writeableRootfsDir:
//     A writeable folder where the rootfs content is.
//   - 'isoMakerArtifactsStagingDir':
//     The folder where the artifacts needed by isoMaker will be staged before
//     'dracut' is run. 'dracut' will include this folder as-is and place it in
//     the initrd image.
//   - 'extraCommandLine':
//     extra kernel command line arguments to add to grub.
//   - 'pxeIsoImageBaseUrl':
//     url to the folder holding the iso to download at boot time.
//     Cannot be specified if pxeIsoImageFileUrl is specified.
//   - 'pxeIsoImageFileUrl':
//     url to the iso image to download at boot time.
//     Cannot be specified if pxeIsoImageBaseUrl is specified.
//   - 'outputImageBase':
//     output image iso name.
//
// outputs
//   - customized writeableRootfsDir (new files, deleted files, etc)
//   - extracted artifacts
func (b *LiveOSIsoBuilder) prepareLiveOSDir(inputSavedConfigsFilePath string, writeableRootfsDir string,
	isoMakerArtifactsStagingDir string, extraCommandLine imagecustomizerapi.KernelExtraArguments, pxeIsoImageBaseUrl string,
	pxeIsoImageFileUrl string, outputImageBase string) error {

	logger.Log.Debugf("Creating LiveOS squashfs image")

	err := b.findKernelVersion(writeableRootfsDir)
	if err != nil {
		return err
	}

	b.artifacts.dracutPackageInfo, err = getDracutVersion(writeableRootfsDir)
	if err != nil {
		return err
	}

	err = b.extractBootDirFiles(writeableRootfsDir)
	if err != nil {
		return err
	}

	exists, err := file.PathExists(inputSavedConfigsFilePath)
	if err != nil {
		return err
	}
	if exists {
		err = file.Copy(inputSavedConfigsFilePath, b.artifacts.savedConfigsFilePath)
		if err != nil {
			return fmt.Errorf("failed to saved arguments file:\n%w", err)
		}
	}

	updatedSavedConfigs, err := updateSavedConfigs(b.artifacts.savedConfigsFilePath, extraCommandLine, pxeIsoImageBaseUrl,
		pxeIsoImageFileUrl, b.artifacts.dracutPackageInfo)
	if err != nil {
		return fmt.Errorf("failed to combine saved configurations with new configuration:\n%w", err)
	}

	err = b.updateGrubCfg(b.artifacts.isoGrubCfgPath, b.artifacts.pxeGrubCfgPath, updatedSavedConfigs, outputImageBase)
	if err != nil {
		return fmt.Errorf("failed to update grub.cfg:\n%w", err)
	}

	err = b.stageIsoMakerInitrdArtifacts(writeableRootfsDir, isoMakerArtifactsStagingDir)
	if err != nil {
		return fmt.Errorf("failed to stage isomaker initrd artifacts:\n%w", err)
	}

	err = b.prepareRootfsForDracut(writeableRootfsDir)
	if err != nil {
		return fmt.Errorf("failed to prepare rootfs for dracut:\n%w", err)
	}

	return nil
}

// createSquashfsImage
//
//	creates a squashfs image based on a given folder.
//
// inputs:
//   - writeableRootfsDir:
//     directory tree root holding the contents to be placed in the squashfs image.
//
// output
//   - creates a squashfs image and stores its path in
//     b.artifacts.squashfsImagePath
func (b *LiveOSIsoBuilder) createSquashfsImage(writeableRootfsDir string) error {

	logger.Log.Debugf("Creating squashfs of %s", writeableRootfsDir)

	squashfsImagePath := filepath.Join(b.workingDirs.isoArtifactsDir, liveOSImage)

	exists, err := file.PathExists(squashfsImagePath)
	if err == nil && exists {
		err = os.Remove(squashfsImagePath)
		if err != nil {
			return fmt.Errorf("failed to delete existing squashfs image (%s):\n%w", squashfsImagePath, err)
		}
	}

	mksquashfsParams := []string{writeableRootfsDir, squashfsImagePath}
	err = shell.ExecuteLive(false, "mksquashfs", mksquashfsParams...)
	if err != nil {
		return fmt.Errorf("failed to create squashfs:\n%w", err)
	}

	b.artifacts.squashfsImagePath = squashfsImagePath

	return nil
}

// generateInitrdImage
//
//	runs dracut against rootfs to create an initrd image file.
//
// inputs:
//   - rootfsSourceDir:
//     local folder (on the build machine) of the rootfs to be used when
//     creating the initrd image.
//   - artifactsSourceDir:
//     source directory (on the build machine) holding an artifacts tree to
//     include in the initrd image.
//   - artifactsTargetDir:
//     target directory (within the initrd image) where the contents of the
//     artifactsSourceDir tree will be copied to.
//
// outputs:
// - creates an initrd.img and stores its path in b.artifacts.initrdImagePath.
func (b *LiveOSIsoBuilder) generateInitrdImage(rootfsSourceDir, artifactsSourceDir, artifactsTargetDir string) error {

	logger.Log.Debugf("Generating initrd")

	chroot := safechroot.NewChroot(rootfsSourceDir, true /*isExistingDir*/)
	if chroot == nil {
		return fmt.Errorf("failed to create a new chroot object for %s.", rootfsSourceDir)
	}
	defer chroot.Close(true /*leaveOnDisk*/)

	err := chroot.Initialize("", nil, nil, true /*includeDefaultMounts*/)
	if err != nil {
		return fmt.Errorf("failed to initialize chroot object for %s:\n%w", rootfsSourceDir, err)
	}

	requiredRpms := []string{"squashfs-tools", "tar", "device-mapper", "curl"}
	for _, requiredRpm := range requiredRpms {
		logger.Log.Debugf("Checking if (%s) is installed", requiredRpm)
		if !isPackageInstalled(chroot, requiredRpm) {
			return fmt.Errorf("package (%s) is not installed:\nthe following packages must be installed to generate an iso: %v", requiredRpm, requiredRpms)
		}
	}

	initrdPathInChroot := "/initrd.img"
	err = chroot.UnsafeRun(func() error {
		dracutParams := []string{
			initrdPathInChroot,
			"--kver", b.artifacts.kernelVersion,
			"--filesystems", "squashfs",
			"--include", artifactsSourceDir, artifactsTargetDir}

		return shell.ExecuteLive(true /*squashErrors*/, "dracut", dracutParams...)
	})
	if err != nil {
		return fmt.Errorf("failed to run dracut:\n%w", err)
	}

	generatedInitrdPath := filepath.Join(rootfsSourceDir, initrdPathInChroot)
	targetInitrdPath := filepath.Join(b.workingDirs.isoArtifactsDir, initrdImage)
	err = file.Copy(generatedInitrdPath, targetInitrdPath)
	if err != nil {
		return fmt.Errorf("failed to copy generated initrd:\n%w", err)
	}
	b.artifacts.initrdImagePath = targetInitrdPath

	return nil
}

// prepareArtifactsFromFullImage
//
//	extracts and generates all LiveOS Iso artifacts from a given raw full disk
//	image (has boot and rootfs partitions).
//
// inputs:
//   - 'inputSavedConfigsFilePath':
//   - 'rawImageFile':
//     path to an existing raw full disk image (i.e. image with boot
//     partition and a rootfs partition).
//   - 'extraCommandLine':
//     extra kernel command line arguments to add to grub.
//   - 'pxeIsoImageBaseUrl':
//     url to the folder holding the iso to download at boot time.
//     Cannot be specified if pxeIsoImageFileUrl is specified.
//   - 'pxeIsoImageFileUrl':
//     url to the iso image to download at boot time.
//     Cannot be specified if pxeIsoImageBaseUrl is specified.
//   - 'outputImageBase':
//     output image iso name.
//
// outputs:
//   - all the extracted/generated artifacts will be placed in the
//     `LiveOSIsoBuilder.workingDirs.isoArtifactsDir` folder.
//   - the paths to individual artifaces are found in the
//     `LiveOSIsoBuilder.artifacts` data structure.
func (b *LiveOSIsoBuilder) prepareArtifactsFromFullImage(inputSavedConfigsFilePath string, rawImageFile string, extraCommandLine imagecustomizerapi.KernelExtraArguments,
	pxeIsoImageBaseUrl string, pxeIsoImageFileUrl string, outputImageBase string) error {

	logger.Log.Infof("Preparing iso artifacts")

	logger.Log.Debugf("Connecting to raw image (%s)", rawImageFile)
	rawImageConnection, err := connectToExistingImage(rawImageFile, b.workingDirs.isoBuildDir, "readonly-rootfs-mount", false /*includeDefaultMounts*/)
	if err != nil {
		return err
	}
	defer rawImageConnection.Close()

	writeableRootfsDir := filepath.Join(b.workingDirs.isoBuildDir, "writeable-rootfs")
	err = b.populateWriteableRootfsDir(rawImageConnection.Chroot().RootDir(), writeableRootfsDir)
	if err != nil {
		return fmt.Errorf("failed to copy the contents of rootfs from image (%s) to local folder (%s):\n%w", rawImageFile, writeableRootfsDir, err)
	}

	isoMakerArtifactsStagingDir := "/boot-staging"
	err = b.prepareLiveOSDir(inputSavedConfigsFilePath, writeableRootfsDir, isoMakerArtifactsStagingDir,
		extraCommandLine, pxeIsoImageBaseUrl, pxeIsoImageFileUrl, outputImageBase)
	if err != nil {
		return fmt.Errorf("failed to convert rootfs folder to a LiveOS folder:\n%w", err)
	}

	err = b.createSquashfsImage(writeableRootfsDir)
	if err != nil {
		return fmt.Errorf("failed to create squashfs image:\n%w", err)
	}

	isoMakerArtifactsDirInInitrd := "/boot"
	err = b.generateInitrdImage(writeableRootfsDir, isoMakerArtifactsStagingDir, isoMakerArtifactsDirInInitrd)
	if err != nil {
		return fmt.Errorf("failed to generate initrd image:\n%w", err)
	}

	return nil
}

// createIsoImage
//
//	creates an LiveOS ISO image.
//
// inputs:
//   - additionalIsoFiles:
//     map of addition files to copy to the iso media.
//     sourcePath -> [ targetPath0, targetPath1, ...]
//   - isoOutputDir:
//     path to a folder where the output image will be placed. It does not
//     need to be created before calling this function.
//   - isoOutputBaseName:
//     path to the iso image to be created upon successful copmletion of this
//     function.
//
// ouptuts:
//   - create a LiveOS ISO.
func (b *LiveOSIsoBuilder) createIsoImage(additionalIsoFiles []safechroot.FileToCopy, isoOutputDir, isoOutputBaseName string) (isoImagePath string, err error) {
	baseDirPath := ""

	// unattended install is where the ISO OS configures a persistent storage
	// and installs RPMs to it. This is different from the LiveOS scenario.
	unattendedInstall := false

	// We are disabling BIOS booloaders because enabling them will requires
	// MIC to take a dependency on binary artifacts stored elsewhere.
	// Should we decide to include the BIOS bootloader, we need to find a
	// reliable and efficient way to pull those binaries.
	enableBiosBoot := false
	isoResourcesDir := ""

	// No stock resources are needed for the LiveOS scenario.
	// No rpms are needed for the LiveOS scenario.
	enableRpmRepo := false
	isoRepoDirPath := ""

	// Construct the output image full path
	isoImageNameInfo := getImageNameFromImageBaseName(isoOutputBaseName)
	isoImagePath = filepath.Join(isoOutputDir, isoImageNameInfo.name)

	// empty target system config since LiveOS does not install the OS
	// artifacts to the target system.
	targetSystemConfig := configuration.Config{}

	// Add the squashfs file
	squashfsImageToCopy := safechroot.FileToCopy{
		Src:  b.artifacts.squashfsImagePath,
		Dest: filepath.Join(liveOSDir, liveOSImage),
	}
	additionalIsoFiles = append(additionalIsoFiles, squashfsImageToCopy)

	// Add /boot/* files
	for sourceFile, targetFile := range b.artifacts.additionalFiles {
		fileToCopy := safechroot.FileToCopy{
			Src:           sourceFile,
			Dest:          targetFile,
			NoDereference: true,
		}
		additionalIsoFiles = append(additionalIsoFiles, fileToCopy)
	}

	// Add the iso saved config file
	exists, err := file.PathExists(b.artifacts.savedConfigsFilePath)
	if err != nil {
		return "", fmt.Errorf("failed to check if (%s) exists:\n%w", b.artifacts.savedConfigsFilePath, err)
	}
	if exists {
		fileToCopy := safechroot.FileToCopy{
			Src:  b.artifacts.savedConfigsFilePath,
			Dest: filepath.Join("/", savedConfigsDir, savedConfigsFileName),
		}
		additionalIsoFiles = append(additionalIsoFiles, fileToCopy)
	}

	// Add the grub-pxe.cfg file
	exists, err = file.PathExists(b.artifacts.pxeGrubCfgPath)
	if err != nil {
		return "", fmt.Errorf("failed to check if (%s) exists:\n%w", b.artifacts.pxeGrubCfgPath, err)
	}
	if exists {
		fileToCopy := safechroot.FileToCopy{
			Src:  b.artifacts.pxeGrubCfgPath,
			Dest: filepath.Join("/", grubCfgDir, pxeGrubCfg),
		}
		additionalIsoFiles = append(additionalIsoFiles, fileToCopy)
	}

	err = os.MkdirAll(isoOutputDir, os.ModePerm)
	if err != nil {
		return "", err
	}

	isoMaker, err := isomakerlib.NewIsoMakerWithConfig(
		unattendedInstall,
		enableBiosBoot,
		enableRpmRepo,
		baseDirPath,
		b.workingDirs.isomakerBuildDir,
		isoImageNameInfo.releaseVersion,
		isoResourcesDir,
		additionalIsoFiles,
		targetSystemConfig,
		isoBootDir,
		b.artifacts.initrdImagePath,
		b.artifacts.isoGrubCfgPath,
		isoRepoDirPath,
		isoOutputDir,
		isoOutputBaseName,
		isoImageNameInfo.tag)
	if err != nil {
		return "", err
	}

	err = isoMaker.Make()
	if err != nil {
		return "", err
	}

	return isoImagePath, nil
}

// micIsoConfigToIsoMakerConfig
//
//	converts imagecustomizerapi.Iso to isomaker configuration.
//
// inputs:
//
//   - 'baseConfigPath'
//     path to the folder where the mic configuration was loaded from.
//     This path will be used to construct absolute paths for build machine
//     file references defined in the config.
//   - 'isoConfig'
//     user provided configuration for the iso image.
//
// outputs:
//   - 'additionalIsoFiles'
//     list of files to copy from the build machine to the iso media.
func micIsoConfigToIsoMakerConfig(baseConfigPath string, isoConfig *imagecustomizerapi.Iso) (additionalIsoFiles []safechroot.FileToCopy, extraCommandLine imagecustomizerapi.KernelExtraArguments, err error) {

	if isoConfig == nil {
		return
	}

	additionalIsoFiles = []safechroot.FileToCopy{}

	for _, additionalFile := range isoConfig.AdditionalFiles {
		absSourceFile := ""
		if additionalFile.Source != "" {
			absSourceFile = file.GetAbsPathWithBase(baseConfigPath, additionalFile.Source)
		}
		fileToCopy := safechroot.FileToCopy{
			Src:         absSourceFile,
			Content:     additionalFile.Content,
			Dest:        additionalFile.Destination,
			Permissions: (*fs.FileMode)(additionalFile.Permissions),
		}
		additionalIsoFiles = append(additionalIsoFiles, fileToCopy)
	}

	return additionalIsoFiles, isoConfig.KernelCommandLine.ExtraCommandLine, nil
}

// createLiveOSIsoImage
//
//	main function to create a LiveOS ISO image from a raw full disk image file.
//
// inputs:
//
//   - 'buildDir':
//     path build directory (can be shared with other tools).
//   - 'baseConfigPath'
//     path to the folder where the mic configuration was loaded from.
//     This path will be used to construct absolute paths for file references
//     defined in the config.
//   - 'inputIsoArtifacts'
//     an optional LiveOSIsoBuilder that holds the state of the original input
//     iso if one was provided. If present, this function will copy all files
//     from the inputIsoArtifacts.artifacts.additionalFiles to the new iso
//     if the destination is not already defined (for the new iso).
//     This is used to carry over any files from a previously customized iso
//     to the new one.
//   - 'isoConfig'
//     user provided configuration for the iso image.
//   - 'pxeConfig'
//     user provided configuration for the PXE flow.
//   - 'rawImageFile':
//     path to an existing raw full disk image (has boot + rootfs partitions).
//   - 'outputImageDir':
//     path to a folder where the generated iso will be placed.
//   - 'outputImageBase':
//     base name of the image to generate. The generated name will be on the
//     form: {outputImageDir}/{outputImageBase}.iso
//   - 'outputPXEArtifactsDir'
//     optional directory path where the PXE artifacts will be exported to if
//     specified.
//
// outputs:
//
//	creates a LiveOS ISO image.
func createLiveOSIsoImage(buildDir, baseConfigPath string, inputIsoArtifacts *LiveOSIsoBuilder, isoConfig *imagecustomizerapi.Iso,
	pxeConfig *imagecustomizerapi.Pxe, rawImageFile, outputImageDir, outputImageBase string, outputPXEArtifactsDir string) (err error) {

	additionalIsoFiles, extraCommandLine, err := micIsoConfigToIsoMakerConfig(baseConfigPath, isoConfig)
	if err != nil {
		return fmt.Errorf("failed to convert iso configuration to isomaker format:\n%w", err)
	}

	pxeIsoImageBaseUrl := ""
	if pxeConfig != nil {
		pxeIsoImageBaseUrl = pxeConfig.IsoImageBaseUrl
	}

	pxeIsoImageFileUrl := ""
	if pxeConfig != nil {
		pxeIsoImageFileUrl = pxeConfig.IsoImageFileUrl
	}

	isoBuildDir := filepath.Join(buildDir, "tmp")
	isoArtifactsDir := filepath.Join(isoBuildDir, "artifacts")
	// IsoMaker needs its own folder to work in (it starts by deleting and re-creating it).
	isomakerBuildDir := filepath.Join(isoBuildDir, "isomaker-tmp")

	isoBuilder := &LiveOSIsoBuilder{
		//
		// buildDir (might be shared with other build tools)
		//  |--tmp   (LiveOSIsoBuilder specific)
		//     |--<various mount points>
		//     |--artifacts        (extracted and generated artifacts)
		//     |--isomaker-tmp     (used exclusively by isomaker)
		//
		workingDirs: IsoWorkingDirs{
			isoBuildDir:      isoBuildDir,
			isoArtifactsDir:  isoArtifactsDir,
			isomakerBuildDir: isomakerBuildDir,
		},
		artifacts: IsoArtifacts{
			savedConfigsFilePath: filepath.Join(isoArtifactsDir, savedConfigsDir, savedConfigsFileName),
		},
	}
	defer func() {
		cleanupErr := os.RemoveAll(isoBuilder.workingDirs.isoBuildDir)
		if cleanupErr != nil {
			if err != nil {
				err = fmt.Errorf("%w:\nfailed to clean-up (%s): %w", err, isoBuilder.workingDirs.isoBuildDir, cleanupErr)
			} else {
				err = fmt.Errorf("failed to clean-up (%s): %w", isoBuilder.workingDirs.isoBuildDir, cleanupErr)
			}
		}
	}()

	// if there is an input iso, make sure to pick-up it's saved kernel args
	// file.
	inputSavedConfigsFilePath := ""
	if inputIsoArtifacts != nil {
		inputSavedConfigsFilePath = inputIsoArtifacts.artifacts.savedConfigsFilePath
	}

	err = isoBuilder.prepareArtifactsFromFullImage(inputSavedConfigsFilePath, rawImageFile, extraCommandLine, pxeIsoImageBaseUrl, pxeIsoImageFileUrl, outputImageBase)
	if err != nil {
		return err
	}

	// If we started from an input iso (not an input vhd(x)/qcow), then there
	// might be additional files that are not defined in the current user
	// configuration. Below, we loop through the files we have captured so far
	// and append any file that was in the input iso and is not included
	// already. This also ensures that no file from the input iso overwrites
	// a newer version that has just been created.
	if inputIsoArtifacts != nil {
		for inputSourceFile, inputTargetFile := range inputIsoArtifacts.artifacts.additionalFiles {
			found := false
			for _, targetFile := range isoBuilder.artifacts.additionalFiles {
				if inputTargetFile == targetFile {
					found = true
					break
				}
			}

			if !found {
				isoBuilder.artifacts.additionalFiles[inputSourceFile] = inputTargetFile
			}
		}
	}

	err = isoBuilder.createIsoImageAndPXEFolder(additionalIsoFiles, outputImageDir, outputImageBase, outputPXEArtifactsDir)
	if err != nil {
		return fmt.Errorf("failed to generate iso image and/or PXE artifacts folder\n%w", err)
	}

	return nil
}

// extractIsoImageContents
//
//   - given an iso image, this function extracts its contents into the specified
//     folder.
//
// inputs:
//
//   - 'buildDir':
//     path build directory (can be shared with other tools).
//   - 'isoImageFile'
//     path to iso image file to extract its contents.
//   - 'isoExpansionFolder'
//     folder where the extracts contents will be copied to.
//
// outputs:
//
//   - creates a local folder with the same structure and contents as the provided
//     iso image.
func extractIsoImageContents(buildDir string, isoImageFile string, isoExpansionFolder string) (err error) {
	mountDir, err := os.MkdirTemp(buildDir, "tmp-iso-mount-")
	if err != nil {
		return fmt.Errorf("failed to create temporary mount folder for iso:\n%w", err)
	}
	defer os.RemoveAll(mountDir)

	isoImageLoopDevice, err := safeloopback.NewLoopback(isoImageFile)
	if err != nil {
		return fmt.Errorf("failed to create loop device for (%s):\n%w", isoImageFile, err)
	}
	defer isoImageLoopDevice.Close()

	isoImageMount, err := safemount.NewMount(isoImageLoopDevice.DevicePath(), mountDir,
		"iso9660" /*fstype*/, unix.MS_RDONLY /*flags*/, "" /*data*/, false /*makeAndDelete*/)
	if err != nil {
		return err
	}
	defer isoImageMount.Close()

	err = os.MkdirAll(isoExpansionFolder, os.ModePerm)
	if err != nil {
		return fmt.Errorf("failed to create folder %s:\n%w", isoExpansionFolder, err)
	}

	err = copyPartitionFiles(mountDir+"/.", isoExpansionFolder)
	if err != nil {
		return fmt.Errorf("failed to copy iso image contents to a writeable folder (%s):\n%w", isoExpansionFolder, err)
	}

	err = isoImageMount.CleanClose()
	if err != nil {
		return err
	}

	err = isoImageLoopDevice.CleanClose()
	if err != nil {
		return err
	}

	return nil
}

// createIsoBuilderFromIsoImage
//
//   - given an iso image, this function extracts its contents, scans them, and
//     constructs a LiveOSIsoBuilder object filling out as many of its fields as
//     possible.
//
// inputs:
//
//   - 'buildDir':
//     path build directory (can be shared with other tools).
//   - 'buildDirAbs'
//     the absolute path of 'buildDir'.
//   - 'isoImageFile'
//     the source iso image file to extract/scan.
//
// outputs:
//
//   - returns an instance of LiveOSIsoBuilder populated with all the paths of the
//     extracted contents.
func createIsoBuilderFromIsoImage(buildDir string, buildDirAbs string, isoImageFile string) (isoBuilder *LiveOSIsoBuilder, err error) {

	isoBuildDir := filepath.Join(buildDir, "tmp")
	isoArtifactsDir := filepath.Join(isoBuildDir, "artifacts")
	// IsoMaker needs its own folder to work in (it starts by deleting and re-creating it).
	isomakerBuildDir := filepath.Join(isoBuildDir, "isomaker-tmp")

	isoBuilder = &LiveOSIsoBuilder{
		//
		// buildDir (might be shared with other build tools)
		//  |--tmp   (LiveOSIsoBuilder specific)
		//     |--<various mount points>
		//     |--artifacts        (extracted and generated artifacts)
		//     |--isomaker-tmp     (used exclusively by isomaker)
		//
		workingDirs: IsoWorkingDirs{
			isoBuildDir:     isoBuildDir,
			isoArtifactsDir: isoArtifactsDir,
			// IsoMaker needs its own folder to work in (it starts by deleting and re-creating it).
			isomakerBuildDir: isomakerBuildDir,
		},
		artifacts: IsoArtifacts{
			savedConfigsFilePath: filepath.Join(isoArtifactsDir, savedConfigsDir, savedConfigsFileName),
		},
	}
	defer func() {
		if err != nil {
			cleanupErr := isoBuilder.cleanUp()
			if cleanupErr != nil {
				err = fmt.Errorf("%w:\nfailed to clean-up:\n%w", err, cleanupErr)
			}
		}
	}()

	// create iso build folder
	err = os.MkdirAll(isoBuildDir, os.ModePerm)
	if err != nil {
		return isoBuilder, fmt.Errorf("failed to create folder %s:\n%w", isoBuildDir, err)
	}
	isoBuilder.addCleanupDir(isoBuildDir)

	// extract iso contents
	isoExpansionFolder, err := os.MkdirTemp(buildDirAbs, "expanded-input-iso-")
	if err != nil {
		return isoBuilder, fmt.Errorf("failed to create a temporary iso expansion folder for iso:\n%w", err)
	}
	isoBuilder.addCleanupDir(isoExpansionFolder)

	err = extractIsoImageContents(buildDir, isoImageFile, isoExpansionFolder)
	if err != nil {
		return isoBuilder, fmt.Errorf("failed to extract iso contents from input iso file:\n%w", err)
	}

	isoFiles, err := file.EnumerateDirFiles(isoExpansionFolder)
	if err != nil {
		return isoBuilder, fmt.Errorf("failed to enumerate expanded iso files under %s:\n%w", isoExpansionFolder, err)
	}

	isoBuilder.artifacts.additionalFiles = make(map[string]string)

	for _, isoFile := range isoFiles {
		fileName := filepath.Base(isoFile)

		scheduleAdditionalFile := true

		switch fileName {
		case bootx64Binary:
			isoBuilder.artifacts.bootx64EfiPath = isoFile
			// isomaker will extract this from initrd and copy it to include it
			// in the iso media - so no need to schedule it as an additional
			// file.
			scheduleAdditionalFile = false
		case grubx64Binary:
			// Note that grubx64NoPrefixBinary is not expected to on an existing
			// iso - and hence we do not look for it here. grubx64NoPrefixBinary
			// may exist only on a vhdx/qcow when the grub-noprefix package is
			// installed. When such images are converted to an iso, we rename
			// the grub binary to its regular name (grubx64.efi).
			isoBuilder.artifacts.grubx64EfiPath = isoFile
			// isomaker will extract this from initrd and copy it to include it
			// in the iso media - so no need to schedule it as an additional
			// file.
			scheduleAdditionalFile = false
		case isoGrubCfg:
			isoBuilder.artifacts.isoGrubCfgPath = isoFile
			// We will place the pxe grub config next to the iso grub config.
			isoBuilder.artifacts.pxeGrubCfgPath = filepath.Join(filepath.Dir(isoBuilder.artifacts.isoGrubCfgPath), pxeGrubCfg)
			// grub.cfg is passed as a parameter to isomaker.
			scheduleAdditionalFile = false
		case liveOSImage:
			isoBuilder.artifacts.squashfsImagePath = isoFile
			// the squashfs image file is added to the additional file list
			// by a different part of the code
			scheduleAdditionalFile = false
		case initrdImage:
			isoBuilder.artifacts.initrdImagePath = isoFile
			// initrd.img is passed as a parameter to isomaker.
			scheduleAdditionalFile = false
		case savedConfigsFileName:
			isoBuilder.artifacts.savedConfigsFilePath = isoFile
			scheduleAdditionalFile = false
		}
		if strings.HasPrefix(fileName, vmLinuzPrefix) {
			isoBuilder.artifacts.vmlinuzPath = isoFile
			// isomaker will extract this from initrd and copy it to include it
			// in the iso media - so no need to schedule it as an additional
			// file.
			scheduleAdditionalFile = false
		}

		if scheduleAdditionalFile {
			isoBuilder.artifacts.additionalFiles[isoFile] = strings.TrimPrefix(isoFile, isoExpansionFolder)
		}
	}

	return isoBuilder, nil
}

// createImageFromUnchangedOS
//
//   - assuming the LiveOSIsoBuilder instance has all its artifacts populated,
//     this function goes straight to updating grub and re-packaging the
//     artifacts into an iso image. It does not re-create the initrd.img or
//     the squashfs.img. This speeds-up customizing iso images when there are
//     no customizations applicable to the OS (i.e. to the squashfs.img).
//
// inputs:
//
//   - 'baseConfigPath':
//     path to where the configuration is loaded from. This is used to resolve
//     relative paths.
//   - 'isoConfig'
//     user provided configuration for the iso image.
//   - 'pxeConfig'
//     user provided configuration for the PXE flow.
//   - 'outputImageDir':
//     path to a folder where the generated iso will be placed.
//   - 'outputImageBase':
//     base name of the image to generate. The generated name will be on the
//     form: {outputImageDir}/{outputImageBase}.iso
//   - 'outputPXEArtifactsDir'
//     optional directory path where the PXE artifacts will be exported to if
//     specified.
//
// outputs:
//
//   - creates an iso image.
func (b *LiveOSIsoBuilder) createImageFromUnchangedOS(baseConfigPath string, isoConfig *imagecustomizerapi.Iso,
	pxeConfig *imagecustomizerapi.Pxe, outputImageDir string, outputImageBase string, outputPXEArtifactsDir string) error {

	logger.Log.Infof("Creating LiveOS iso image using unchanged OS partitions")

	additionalIsoFiles, extraCommandLine, err := micIsoConfigToIsoMakerConfig(baseConfigPath, isoConfig)
	if err != nil {
		return fmt.Errorf("failed to convert iso configuration to isomaker configuration format:\n%w", err)
	}

	pxeIsoImageBaseUrl := ""
	if pxeConfig != nil {
		pxeIsoImageBaseUrl = pxeConfig.IsoImageBaseUrl
	}

	pxeIsoImageFileUrl := ""
	if pxeConfig != nil {
		pxeIsoImageFileUrl = pxeConfig.IsoImageFileUrl
	}

	updatedSavedConfigs, err := updateSavedConfigs(b.artifacts.savedConfigsFilePath, extraCommandLine, pxeIsoImageBaseUrl,
		pxeIsoImageFileUrl, b.artifacts.dracutPackageInfo)
	if err != nil {
		return fmt.Errorf("failed to combine saved configurations with new configuration:\n%w", err)
	}

	// Need to populate the dracut package information from the saved copy
	// since we will not expand the rootfs and inspect its contents to get
	// such information.
	b.artifacts.dracutPackageInfo = updatedSavedConfigs.OS.DracutPackageInfo

	err = b.updateGrubCfg(b.artifacts.isoGrubCfgPath, b.artifacts.pxeGrubCfgPath, updatedSavedConfigs, outputImageBase)
	if err != nil {
		return fmt.Errorf("failed to update grub.cfg:\n%w", err)
	}

	err = b.createIsoImageAndPXEFolder(additionalIsoFiles, outputImageDir, outputImageBase, outputPXEArtifactsDir)
	if err != nil {
		return fmt.Errorf("failed to generate iso image and/or PXE artifacts folder\n%w", err)
	}

	return nil
}

// createIsoImageAndPXEFolder
//
//   - This function create the liveos iso image and also populates the PXE
//     artifacts folder.
//
// inputs:
//
//   - additionalIsoFiles:
//     map of addition files to copy to the iso media.
//     sourcePath -> [ targetPath0, targetPath1, ...]
//   - outputImageDir:
//     path to a folder where the output image will be placed. It does not
//     need to be created before calling this function.
//   - outputImageBase:
//     path to the iso image to be created upon successful copmletion of this
//     function.
//   - 'outputPXEArtifactsDir'
//     path to the output directory where the extract artifacts will be saved to.
//
// outputs:
//
//   - create an iso image.
//   - creates a folder with PXE artifacts.
func (b *LiveOSIsoBuilder) createIsoImageAndPXEFolder(additionalIsoFiles []safechroot.FileToCopy, outputImageDir string,
	outputImageBase string, outputPXEArtifactsDir string) error {
	isoImagePath, err := b.createIsoImage(additionalIsoFiles, outputImageDir, outputImageBase)
	if err != nil {
		return err
	}

	if outputPXEArtifactsDir != "" {
		err = verifyDracutPXESupport(b.artifacts.dracutPackageInfo)
		if err != nil {
			return fmt.Errorf("cannot generate the PXE artifacts folder.\n%w", err)
		}
		err = populatePXEArtifactsDir(isoImagePath, b.workingDirs.isoBuildDir, outputPXEArtifactsDir, outputImageBase)
		if err != nil {
			return err
		}
	}

	return nil
}

// populatePXEArtifactsDir
//
//   - This function takes in an liveos iso, and extracts its artifacts unto a
//     folder for easier copying to a PXE server later by the user.
//   - It also renames the liveos iso grub-pxe.cfg to grub.cfg.
//
// inputs:
//
//   - 'isoImagePath':
//     path to a liveos iso image.
//   - 'buildDir'
//     path to a directory to hold intermediate files.
//   - 'outputPXEArtifactsDir'
//     path to the output directory where the extract artifacts will be saved to.
//   - 'outputImageBase':
//     base name of the image to generate. The generated name will be on the
//     form: {outputImageDir}/{outputImageBase}.iso
//
// outputs:
//
//   - creates a folder with PXE artifacts.
func populatePXEArtifactsDir(isoImagePath string, buildDir string, outputPXEArtifactsDir string, outputImageBase string) error {

	logger.Log.Infof("Copying PXE artifacts to (%s)", outputPXEArtifactsDir)

	// Ensure output folder is clean.
	err := os.RemoveAll(outputPXEArtifactsDir)
	if err != nil {
		return fmt.Errorf("failed to remove (%s):\n%w", outputPXEArtifactsDir, err)
	}

	// Extract all files from the iso image file.
	err = extractIsoImageContents(buildDir, isoImagePath, outputPXEArtifactsDir)
	if err != nil {
		return err
	}

	// Replace the iso grub.cfg with the PXE grub.cfg
	isoGrubCfgPath := filepath.Join(outputPXEArtifactsDir, grubCfgDir, isoGrubCfg)
	pxeGrubCfgPath := filepath.Join(outputPXEArtifactsDir, grubCfgDir, pxeGrubCfg)
	err = file.Copy(pxeGrubCfgPath, isoGrubCfgPath)
	if err != nil {
		return fmt.Errorf("failed to copy (%s) to (%s) while populating the PXE artifacts directory:\n%w", pxeGrubCfgPath, isoGrubCfgPath, err)
	}

	err = os.RemoveAll(pxeGrubCfgPath)
	if err != nil {
		return fmt.Errorf("failed to remove file (%s):\n%w", pxeGrubCfgPath, err)
	}

	// Move bootloader files from under '<pxe-folder>/efi/boot' to '<pxe-folder>/'
	bootloaderSrcDir := filepath.Join(outputPXEArtifactsDir, isoBootloadersDir)
	bootloaderFiles := []string{bootx64Binary, grubx64Binary}
	for _, bootloaderFile := range bootloaderFiles {
		sourcePath := filepath.Join(bootloaderSrcDir, bootloaderFile)
		targetPath := filepath.Join(outputPXEArtifactsDir, bootloaderFile)
		err = file.Move(sourcePath, targetPath)
		if err != nil {
			return fmt.Errorf("failed to move boot loader file from (%s) to (%s) while generated the PXE artifacts folder:\n%w", sourcePath, targetPath, err)
		}
	}

	// Remove the empty 'pxe-folder>/efi' folder.
	isoEFIDir := filepath.Join(outputPXEArtifactsDir, "efi")
	err = os.RemoveAll(isoEFIDir)
	if err != nil {
		return fmt.Errorf("failed to remove folder (%s):\n%w", isoEFIDir, err)
	}

	// The iso image file itself must be placed in the PXE folder because
	// dracut livenet module will download it.
	artifactsIsoImagePath := filepath.Join(outputPXEArtifactsDir, getImageNameFromImageBaseName(outputImageBase).name)
	err = file.Copy(isoImagePath, artifactsIsoImagePath)
	if err != nil {
		return fmt.Errorf("failed to copy (%s) while populating the PXE artifacts directory:\n%w", isoImagePath, err)
	}

	return nil
}

// getSizeOnDiskInBytes
//
//   - given a folder, it calculates the total size in bytes of its contents.
//
// inputs:
//
//   - 'rootDir':
//     root folder to calculate its size.
//
// outputs:
//
//   - returns the size in bytes.
func getSizeOnDiskInBytes(rootDir string) (size uint64, err error) {
	logger.Log.Debugf("Calculating total size for (%s)", rootDir)

	duStdout, _, err := shell.Execute("du", "-s", rootDir)
	if err != nil {
		return 0, fmt.Errorf("failed to find the size of the specified folder using 'du' for (%s):\n%w", rootDir, err)
	}

	// parse and get count and unit
	diskSizeRegex := regexp.MustCompile(`^(\d+)\s+`)
	matches := diskSizeRegex.FindStringSubmatch(duStdout)
	if matches == nil || len(matches) < 2 {
		return 0, fmt.Errorf("failed to parse 'du -s' output (%s).", duStdout)
	}

	sizeInKbsString := matches[1]
	sizeInKbs, err := strconv.ParseUint(sizeInKbsString, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("failed to parse disk size (%d):\n%w", sizeInKbs, err)
	}

	return sizeInKbs * diskutils.KiB, nil
}

// getDiskSizeEstimateInMBs
//
//   - given a folder, it calculates the size of a disk image that can hold
//     all of its contents.
//   - The amount of disk space a file occupies depends on the block size of the
//     host file system. If many files are smaller than a block size, there will
//     be a lot of waste. If files are very large, there will be very little
//     waste. It is hard to predict how much disk space a set of a files will
//     occupy without enumerating the sizes of all the files and knowing the
//     target block size. In this function, we use an optimistic approach which
//     calculates the required disk space by multiplying the total file size by
//     a safety factor - i.e. safe that it will be able t hold all the contents.
//
// inputs:
//
//   - 'rootDir':
//     root folder to calculate its size.
//   - 'safetyFactor':
//     a multiplier used with the total number of bytes calculated.
//
// outputs:
//
//   - returns the size in mega bytes.
func getDiskSizeEstimateInMBs(rootDir string, safetyFactor float64) (size uint64, err error) {

	sizeInBytes, err := getSizeOnDiskInBytes(rootDir)
	if err != nil {
		return 0, fmt.Errorf("failed to get folder size on disk while estimating total disk size:\n%w", err)
	}

	sizeInMBs := sizeInBytes/diskutils.MiB + 1
	estimatedSizeInMBs := uint64(float64(sizeInMBs) * safetyFactor)
	return estimatedSizeInMBs, nil
}

// createWriteableImageFromSquashfs
//
//   - given a squashfs image file, it creates a writeable image with two
//     partitions, and copies the contents of the squashfs unto that writeable
//     image.
//   - the squashfs image file must be extracted from a previously created
//     LiveOS iso and is specified by the LiveOSIsoBuilder.artifacts.squashfsImagePath.
//
// inputs:
//
//   - 'buildDir':
//     path build directory (can be shared with other tools).
//   - 'rawImageFile':
//     the name of the raw image to create and populate with the contents of
//     the squashfs.
//
// outputs:
//
//   - creates the specified writeable image.
func (b *LiveOSIsoBuilder) createWriteableImageFromSquashfs(buildDir, rawImageFile string) error {

	logger.Log.Infof("Creating writeable image from squashfs (%s)", b.artifacts.squashfsImagePath)

	// mount squash fs
	squashMountDir, err := os.MkdirTemp(buildDir, "tmp-squashfs-mount-")
	if err != nil {
		return fmt.Errorf("failed to create temporary mount folder for squashfs:\n%w", err)
	}
	defer os.RemoveAll(squashMountDir)

	squashfsLoopDevice, err := safeloopback.NewLoopback(b.artifacts.squashfsImagePath)
	if err != nil {
		return fmt.Errorf("failed to create loop device for (%s):\n%w", b.artifacts.squashfsImagePath, err)
	}
	defer squashfsLoopDevice.Close()

	isoImageMount, err := safemount.NewMount(squashfsLoopDevice.DevicePath(), squashMountDir,
		"squashfs" /*fstype*/, 0 /*flags*/, "" /*data*/, false /*makeAndDelete*/)
	if err != nil {
		return err
	}
	defer isoImageMount.Close()

	// estimate the new disk size
	safeDiskSizeMB, err := getDiskSizeEstimateInMBs(squashMountDir, expansionSafetyFactor)
	if err != nil {
		return fmt.Errorf("failed to calculate the disk size of %s:\n%w", squashMountDir, err)
	}

	logger.Log.Debugf("safeDiskSizeMB = %d", safeDiskSizeMB)

	// define a disk layout with a boot partition and a rootfs partition
	maxDiskSizeMB := imagecustomizerapi.DiskSize(safeDiskSizeMB * diskutils.MiB)
	bootPartitionStart := imagecustomizerapi.DiskSize(1 * diskutils.MiB)
	bootPartitionEnd := imagecustomizerapi.DiskSize(9 * diskutils.MiB)

	diskConfig := imagecustomizerapi.Disk{
		PartitionTableType: imagecustomizerapi.PartitionTableTypeGpt,
		MaxSize:            &maxDiskSizeMB,
		Partitions: []imagecustomizerapi.Partition{
			{
				Id:    "esp",
				Start: &bootPartitionStart,
				End:   &bootPartitionEnd,
				Type:  imagecustomizerapi.PartitionTypeESP,
			},
			{
				Id:    "rootfs",
				Start: &bootPartitionEnd,
			},
		},
	}

	fileSystemConfigs := []imagecustomizerapi.FileSystem{
		{
			DeviceId:    "esp",
			PartitionId: "esp",
			Type:        imagecustomizerapi.FileSystemTypeFat32,
			MountPoint: &imagecustomizerapi.MountPoint{
				Path:    "/boot/efi",
				Options: "umask=0077",
			},
		},
		{
			DeviceId:    "rootfs",
			PartitionId: "rootfs",
			Type:        imagecustomizerapi.FileSystemTypeExt4,
			MountPoint: &imagecustomizerapi.MountPoint{
				Path: "/",
			},
		},
	}

	// populate the newly created disk image with content from the squash fs
	installOSFunc := func(imageChroot *safechroot.Chroot) error {
		// At the point when this copy will be executed, both the boot and the
		// root partitions will be mounted, and the files of /boot/efi will
		// land on the the boot partition, while the rest will be on the rootfs
		// partition.
		err := copyPartitionFiles(squashMountDir+"/.", imageChroot.RootDir())
		if err != nil {
			return fmt.Errorf("failed to copy squashfs contents to a writeable disk:\n%w", err)
		}
		return err
	}

	// create the new raw disk image
	writeableChrootDir := "writeable-raw-image"
	_, err = createNewImage(rawImageFile, diskConfig, fileSystemConfigs, buildDir, writeableChrootDir, installOSFunc)
	if err != nil {
		return fmt.Errorf("failed to copy squashfs into new writeable image (%s):\n%w", rawImageFile, err)
	}

	err = isoImageMount.CleanClose()
	if err != nil {
		return err
	}

	err = squashfsLoopDevice.CleanClose()
	if err != nil {
		return err
	}

	return nil
}
