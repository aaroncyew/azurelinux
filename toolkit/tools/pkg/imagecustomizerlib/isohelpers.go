// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package imagecustomizerlib

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"

	"github.com/microsoft/CBL-Mariner/toolkit/tools/internal/file"
	"github.com/microsoft/CBL-Mariner/toolkit/tools/internal/logger"
	"github.com/microsoft/CBL-Mariner/toolkit/tools/internal/safemount"
	"github.com/microsoft/CBL-Mariner/toolkit/tools/internal/safechroot"
	"github.com/microsoft/CBL-Mariner/toolkit/tools/internal/shell"
	"github.com/microsoft/CBL-Mariner/toolkit/tools/imagegen/configuration"
	"github.com/microsoft/CBL-Mariner/toolkit/tools/pkg/isomakerlib"
)

var (
	grubCfgTemplate = `set default="0"
set timeout=0

menuentry "Mariner Baremetal Iso" {

	search --label CDROM --set root
	linux /isolinux/vmlinuz \
			overlay-size=70% \
			selinux=0 \
			console=tty0 \
			apparmor=0 \
			root=live:LABEL=CDROM \
			rd.shell \
			rd.live.image \
			rd.live.dir=config/additionalfiles/0 \
			rd.live.squashimg=rootfs.img \
			rd.live.overlay=1 \
			rd.live.overlay.nouserconfirmprompt

	initrd /isolinux/initrd.img
}	
`
	dracutConfig = `add_dracutmodules+=" dmsquash-live "
add_drivers+=" overlay "
`
)

type IsoWorkingDirs struct {
	isoBuildDir      string
	// 'isomakerBuildDir' will be deleted/re-created by IsoMaker before it
	// proceeds. It needs to be different from `isoBuildDir`.
	isomakerBuildDir string
	outDir 	         string
}

// `IsoArtifacts` holds the extracted/generated artifacts necessary to build
// a LiveOS ISO image.
type IsoArtifacts struct {
	kernelVersion     string
	bootx64EfiPath    string
	grubx64EfiPath    string
	grubCfgPath       string
	vmlinuzPath       string
	initrdPath        string
	squashfsImagePath string
}

type IsoArtifactExtractor struct {
	workingDirs    IsoWorkingDirs
	artifacts      IsoArtifacts
}

// purpose:
//   runs dracut against rootfs to create an initrd image file.
//
// inputs:
//   - rootfsSourceDir [in]:
//     - local folder (on the build machine) of the rootfs to be used when
//       creating the initrd image.
//   - artifactsSourceDir [in]:
//     - source directory (on the build machine) holding an artifacts tree to
//       include in the initrd image.
//   - artifactsTargetDir [in]:
//     - target directory (within the initrd image) where the contents of the
//       artifactsSourceDir tree will be copied to.
//
// outputs:
// - creates an initrd.img and stores its path in iae.artifacts.initrdPath.
//
func (iae* IsoArtifactExtractor) generateInitrd(rootfsSourceDir, artifactsSourceDir, artifactsTargetDir string) error {

	logger.Log.Infof("generating initrd...")

	chroot := safechroot.NewChroot(rootfsSourceDir, true /*isExistingDir*/)
	if chroot == nil {
		return fmt.Errorf("failed to create a new chroot object for %s.", rootfsSourceDir)
	}
	defer chroot.Close(true /*leaveOnDisk*/)

	err := chroot.Initialize("", nil, nil, true /*includeDefaultMounts*/)
	if err != nil {
		return fmt.Errorf("failed to initialize chroot object for %s.\n%w", rootfsSourceDir, err)
	}

	initrdPathInChroot := "/initrd.img"
	err = chroot.Run(func() error {
		dracutParams := []string{
			initrdPathInChroot,
			"--kver", iae.artifacts.kernelVersion,
			"--filesystems", "squashfs",
			"--include", artifactsSourceDir, artifactsTargetDir }

		return shell.ExecuteLive(false /*squashErrors*/, "dracut", dracutParams...)
	})
	if err != nil {
		return fmt.Errorf("failed to run dracut.\n%w", err)
	}

	generatedInitrdPath := filepath.Join(rootfsSourceDir, initrdPathInChroot)
	targetInitrdPath := filepath.Join(iae.workingDirs.outDir, "initrd.img")
	err = copyFile(generatedInitrdPath, targetInitrdPath)
	if err != nil {
		return fmt.Errorf("failed to copy generated initrd.\n%w", err)
	}
	iae.artifacts.initrdPath = targetInitrdPath

	return nil
}

// purpose:
//   creates an IsoMaker config objects with the necessary configuration.
//
// inputs:
//   - squashfsImagePath [in]:
//     - path to an existing squashfs image file. The configuration will instruct
//       IsoMaker to place it under:
//       - /config/additionalfiles/0/$(basename $squashfsImagePath).
//
// outputs:
//   - returns an IsoMaker configuration.Config object.
//
func createIsoMakerConfig(squashfsImagePath string) (configuration.Config, error) {

	config := configuration.Config{
		SystemConfigs: []configuration.SystemConfig{
			{
				AdditionalFiles: map[string]configuration.FileConfigList{
					// 'AdditionalFiles' is meant to do two things:
					// 1. copy the files from the build machine to the ISO
					//    media.
					// 2. have Mariner installer copy those files from the ISO
					//    media to the target storage device.
					// In the MIC LiveOS ISO generation sceanrio, we do not
					// have/run Mariner installer and do not need to copy them.
					// So, we are setting the destination to 'dummy-name' as it
					// never be used.
					squashfsImagePath: {{Path: "/dummy-name"}},
				},
			},
		},
	}

	return config, nil
}

// purpose:
//   creates an LiveOS ISO image.
//
// inputs:
//   - isomakerBuildDir [in]:
//	   - folder to be created by the IsoMaker tool to place its temporary files.
//   - grubCfgPath [in]:
//     - path to the grub.cfg file to be used with the bootloaders.
//   - initrdImagePath [in]:
//     - path to an existing initrd image file. The initrd image must be
//       configured to run the LiveOS booting flow in Dracut.
//   - squashfsImagePath [in]:
//     - path to an existing squashfs image file. The squashfs must host a
//       rootfs so that initrd can pivot.
//   - isoOutputDir [in]:
//     - path to a folder where the output image will be placed. It does not
//       need to be created before calling this function.
//   - isoOutputBaseName [in]:
//     - path to the iso image to be created upon successful copmletion of this
//       function.
//
// ouptuts:
//   - create a LiveOS ISO.
//
func createLiveOSISO(isomakerBuildDir, grubCfgPath, initrdImagePath, squashfsImagePath, isoOutputDir, isoOutputBaseName string) error {

	logger.Log.Infof("creating iso...")
	logger.Log.Infof("- isomakerBuildDir  = %s", isomakerBuildDir)
	logger.Log.Infof("- grubCfgPath       = %s", grubCfgPath)
	logger.Log.Infof("- initrdImagePath   = %s", initrdImagePath)
	logger.Log.Infof("- squashfsImagePath = %s", squashfsImagePath)
	logger.Log.Infof("- isoOutputDir      = %s", isoOutputDir)
	logger.Log.Infof("- isoOutputBaseName = %s", isoOutputBaseName)

	unattendedInstall := false
	// We are disabling BIOS booloaders because enabling them will requires
	// MIC to take a dependency on binary artifacts stored elsewhere.
	// Should we decide to include the BIOS bootloader, we need to find a
	// reliable and efficient way to pull those binaries.
	enableBiosBoot := false
	baseDirPath := ""
	releaseVersion := ""
	isoResourcesDir := ""
	isoRepoDirPath := ""
	imageNameTag := ""

	config, err := createIsoMakerConfig(squashfsImagePath)
	if err != nil {
		return err
	}

	err = os.MkdirAll(isoOutputDir, os.ModePerm)
	if err != nil {
		return err
	}

	// isoMaker constructs the final image name as follows:
	// {isoOutputDir}/{isoOutputBaseName}{releaseVersion}{imageNameTag}.iso

	isoMaker := isomakerlib.NewIsoMakerWithConfig(
		unattendedInstall,
		enableBiosBoot,
		baseDirPath,
		isomakerBuildDir,
		releaseVersion,
		isoResourcesDir,
		config,
		initrdImagePath,
		grubCfgPath,
		isoRepoDirPath,
		isoOutputDir,
		isoOutputBaseName,
		imageNameTag)

	isoMaker.Make()

	return nil
}


// purpose:
//   extracts the bootloaders from the specified boot device.
//
// inputs:
//   - 'bootDevicePath': path to an existing boot device.
//   - 'bootfsType': file system type of the specified boot device.
//
// output:
//   the bootloaders are saved to the iae.workingDirs.isoBuildDir
//
func (iae* IsoArtifactExtractor) extractArtifactsFromBootDevice(bootDevicePath string, bootfsType string) (error) {

	logger.Log.Infof("extracting artifacts from the boot partition...")

	loopDevMountFullDir := filepath.Join(iae.workingDirs.isoBuildDir, "readonly-boot-mount")
	logger.Log.Infof("mounting %s(%s) to %s", bootDevicePath, bootfsType, loopDevMountFullDir)

	fullDiskBootMount, err := safemount.NewMount(bootDevicePath, loopDevMountFullDir, bootfsType, 0, "", true)
	if err != nil {
		return fmt.Errorf("failed to mount boot partition (%s):\n%w", bootDevicePath, err)
	}
	defer fullDiskBootMount.Close()

	sourceBootx64EfiPath := filepath.Join(loopDevMountFullDir, "/EFI/BOOT/bootx64.efi")
	targetBootx64EfiPath := filepath.Join(iae.workingDirs.outDir, "bootx64.efi")
	err = copyFile(sourceBootx64EfiPath, targetBootx64EfiPath)
	if err != nil {
		return fmt.Errorf("failed to copy bootloader file (bootx64.efi).\n%w", err)
	}
	iae.artifacts.bootx64EfiPath = targetBootx64EfiPath

	sourceGrubx64EfiPath := filepath.Join(loopDevMountFullDir, "/EFI/BOOT/grubx64.efi")
	targetGrubx64EfiPath := filepath.Join(iae.workingDirs.outDir, "grubx64.efi")
	err = copyFile(sourceGrubx64EfiPath, targetGrubx64EfiPath)
	if err != nil {
		return fmt.Errorf("failed to copy bootloader file (grubx64.efi).\n%w", err)
	}
	iae.artifacts.grubx64EfiPath = targetGrubx64EfiPath

	return nil
}

// purpose:
//   copies the contents of the rootfs partition unto the build machine.
//
// input:
//   - 'rootfsDevicePath' [in]
//     - path to an existing device - where the device holds a roootfs.
//   - 'rootfsType' [in]
//     - the file system type of the specified device.
//   - 'writeableRootfsDir'
//     - path to the folder where the contents of the rootfsDevice will be
//       copied to.
//
func (iae* IsoArtifactExtractor) populateWriteableRootfsDir(rootfsDevicePath, rootfsType, writeableRootfsDir string) (error) {

	logger.Log.Infof("creating writeable rootfs...")

	sourceMountDir := filepath.Join(iae.workingDirs.isoBuildDir, "readonly-rootfs-mount")
	logger.Log.Infof("mounting %s(%s) to %s", rootfsDevicePath, rootfsType, sourceMountDir)

	loopDevMount, err := safemount.NewMount(rootfsDevicePath, sourceMountDir, rootfsType, 0, "", true)
	if err != nil {
		return fmt.Errorf("failed to mount rootfs partition (%s):\n%w", rootfsDevicePath, err)
	}
	defer loopDevMount.Close()

	err = os.MkdirAll(writeableRootfsDir, os.ModePerm)
	if err != nil {
		return fmt.Errorf("failed to create folder %s.\n%w", writeableRootfsDir, err)
	}

	logger.Log.Infof("copying from %s to %s", sourceMountDir, writeableRootfsDir)
	cpParams := []string{"-aT", sourceMountDir, writeableRootfsDir}
	err = shell.ExecuteLive(false, "cp", cpParams...)
	if err != nil {
		return fmt.Errorf("failed to copy rootfs contents to a writeable folder (%s).\n%w", writeableRootfsDir, err)
	}

	return nil
}

// purpose
//  IsoMaker looks for the vmlinuz/bootloader files inside the initrd image
//  file under specific directory structure.
//  This function extracts those artifacts and places them under the same
//  directory structure expected by IsoMaker.
//  This is a staging steps until we run 'dracut' which will take this
//  directory structure and embeds it into the initrd image.
//  Finaly, the IsoMaker will read the initrd image and find the artifacts
//  it needs.
//  Something to consider in the future, change IsoMaker so that it can pick
//  those artifacts from the build machine directly.
//
// inputs:
//   - 'writeableRootfsDir':
//     - path to an existing folder holding the contents of the rootfs.
//   - 'isoMakerArtifactsStagingDir'
//     - path to a folder where the extracted artifacts will stored under.
//
// outputs:
//  the artifacts will be stored in 'isoMakerArtifactsStagingDir'.
//
func (iae* IsoArtifactExtractor) stageIsoMakerInitrdArtifacts(writeableRootfsDir, isoMakerArtifactsStagingDir string) (error) {

	logger.Log.Infof("staging isomaker artifacts into writeable image...")

	targetBootloadersInChroot:=filepath.Join(isoMakerArtifactsStagingDir, "/efi/EFI/BOOT")
	targetBootloadersDir := filepath.Join(writeableRootfsDir, targetBootloadersInChroot)

	err := os.MkdirAll(targetBootloadersDir, os.ModePerm)
	if err != nil {
		return fmt.Errorf("failed to create %s\n%w", targetBootloadersDir, err)
	}

	sourceBoot64EfiPath := filepath.Join(iae.workingDirs.outDir, "bootx64.efi")
	targetBoot64EfiPath := filepath.Join(targetBootloadersDir, "bootx64.efi")
	err = copyFile(sourceBoot64EfiPath, targetBoot64EfiPath)
	if err != nil {
		return fmt.Errorf("failed to bootloader file (bootx64.efi).\n%w", err)
	}

	sourceGrub64EfiPath := filepath.Join(iae.workingDirs.outDir, "grubx64.efi")
	targetGrub64EfiPath := filepath.Join(targetBootloadersDir, "grubx64.efi")
	err = copyFile(sourceGrub64EfiPath, targetGrub64EfiPath)
	if err != nil {
		return fmt.Errorf("failed to bootloader file (grubx64.efi).\n%w", err)
	}

	targetVmlinuzLocalDir := filepath.Join(writeableRootfsDir, isoMakerArtifactsStagingDir)

	sourceVmlinuzPath := iae.artifacts.vmlinuzPath
	targetVmlinuzPath := filepath.Join(targetVmlinuzLocalDir, "vmlinuz")
	err = copyFile(sourceVmlinuzPath, targetVmlinuzPath)
	if err != nil {
		return fmt.Errorf("failed to stage vmlinuz.\n%w", err)
	}

	return nil
}

// purpose
//  ensures two things:
//  - initrd image build time configuration is in place.
//  - rootfs (squashfs) image contents are compatible with our LiveOS initrd
//    boot flow.
//  note that the same rootfs is used for both:
//  (1) creating the initrd image and
//  (2) creating the squashfs image.
//
// inputs
// - writeableRootfsDir [in]:
//   - root directory of existing rootfs content to modify.
//
// outputs:
// - all changes will be applied to the specified rootfs directory in the input.
//
func (iae* IsoArtifactExtractor) prepareRootfsForDracut(writeableRootfsDir string) (error) {

	logger.Log.Infof("preparing writeable image for dracut...")

	fstabFile := filepath.Join(writeableRootfsDir, "/etc/fstab")
	logger.Log.Infof("deleting fstab from %s", fstabFile)
	err := os.Remove(fstabFile)
	if err != nil {
		return fmt.Errorf("failed to delete fstab.\n%w", err)
	}

	sourceConfigFile := filepath.Join(iae.workingDirs.isoBuildDir, "20-live-cd.conf")
	err = ioutil.WriteFile(sourceConfigFile, []byte(dracutConfig), 0644)
	if err != nil {
		return fmt.Errorf("failed to create %s.\n%w", sourceConfigFile, err)
	}

	targetConfigFile := filepath.Join(writeableRootfsDir, "/etc/dracut.conf.d/20-live-cd.conf")
	err = copyFile(sourceConfigFile, targetConfigFile)
	if err != nil {
		return fmt.Errorf("failed to copy dracut config at %s.\n%w", targetConfigFile, err)
	}

	return nil
}

// purpose
//   creates a squashfs image based on a given folder.
// inputs
//   writeableRootfsDir [in]:
//   - directory tree root holding the contents to be placed in the squashfs image.
//
// output
//   - creates a squashfs image and stores its path in 
//     iae.artifacts.squashfsImagePath
//
func (iae* IsoArtifactExtractor) createSquashfsImage(writeableRootfsDir string) (error) {

	logger.Log.Infof("creating squashfs of %s", writeableRootfsDir)

	squashfsImagePath := filepath.Join(iae.workingDirs.outDir, "rootfs.img")

	exists, err := fileExists(squashfsImagePath)
	if err == nil && exists {
		err = os.Remove(squashfsImagePath)
		if err != nil {
			return fmt.Errorf("failed to delete existing squashfs image (%s).\r%w", squashfsImagePath, err)
		}
	}

	mksquashfsParams := []string{writeableRootfsDir, squashfsImagePath}
	err = shell.ExecuteLive(false, "mksquashfs", mksquashfsParams...)
	if err != nil {
		return fmt.Errorf("failed to create squashfs.\r%w", err)
	}

	iae.artifacts.squashfsImagePath = squashfsImagePath
	return nil
}

// purpose
//  given a rootfs, it
//  - extracts the kernel version, vmlinuz, and grub.cfg. 
//  - stages files to be included in the initrd when it's generated. IsoMaker
//    expects those files to be embedded in the initrd.
//  - prepares the rootfs to run dracut (dracut will generate the initrd later).
//  - creates the squashfs.
// inputs
// - writeableRootfsDir [in]:
//     A writeable folder where the rootfs content is.
// - isoMakerArtifactsStagingDir [in]:
//     The folder where the artifacts needed by isoMaker will be staged before
//     'dracut' is run. 'dracut' will include this folder as-is and place it in
//     the initrd image.
//
// outputs
//
func (iae* IsoArtifactExtractor) prepareLiveOSDir(writeableRootfsDir, isoMakerArtifactsStagingDir string) (error) {

	logger.Log.Infof("creating LiveOS squashfs image...")

	// extract kernel version
	kernelParentPath := filepath.Join(writeableRootfsDir, "/usr/lib/modules")
	kernelPaths, err := os.ReadDir(kernelParentPath)
	if err != nil {
		return fmt.Errorf("failed to enumerate kernels under (%s).\n%w", kernelParentPath, err)
	}
	if len(kernelPaths) == 0 {
		return fmt.Errorf("did not find any installed kernels under (%s).\n%w", kernelParentPath, err)
	}
	// do we need to sort this?
	iae.artifacts.kernelVersion = kernelPaths[len(kernelPaths)-1].Name()
	logger.Log.Infof("found installed kernel version (%s)", iae.artifacts.kernelVersion)	

	// create grub.cfg
	targetGrubCfgPath := filepath.Join(iae.workingDirs.outDir, "grub.cfg")

	err = ioutil.WriteFile(targetGrubCfgPath, []byte(grubCfgTemplate), 0644)
	if err != nil {
		return fmt.Errorf("failed to create grub.cfg.\n%w", err)
	}

	iae.artifacts.grubCfgPath = targetGrubCfgPath

	// extract/create vmlinuz
	sourceVmlinuzPath := filepath.Join(writeableRootfsDir, "/boot/vmlinuz-" + iae.artifacts.kernelVersion)
	targetVmLinuzPath := filepath.Join(iae.workingDirs.outDir, "vmlinuz")

	err = copyFile(sourceVmlinuzPath, targetVmLinuzPath)
	if err != nil {
		return fmt.Errorf("failed to extract vmlinuz from (%s).\n%w", sourceVmlinuzPath, err)
	}

	iae.artifacts.vmlinuzPath = targetVmLinuzPath

	// stage artifacts needed by isomaker
	// the artifacts are staged inside initrd where isomaker will look for them
	// when creating the final iso image.
	err = iae.stageIsoMakerInitrdArtifacts(writeableRootfsDir, isoMakerArtifactsStagingDir)
	if err != nil {
		return fmt.Errorf("failed to stage isomaker initrd artifacts.\n%w", err)
	}

	// configure dracut
	err = iae.prepareRootfsForDracut(writeableRootfsDir)
	if err != nil {
		return fmt.Errorf("failed to prepare rootfs for dracut.\n%w", err)
	}

	return nil
}

// purpose
//   helper that ensures the target folder exists before copying the file to
//   it.
//
// inputs:
//   'sourcePath' [in]:
//   - path to the source file.
//   'targetPath' [in]:
//   - path to the target file.
//
// outputs:
//   the source file is copies to the target path.
//
func copyFile(sourcePath, targetPath string) error {

	logger.Log.Infof("copying %s to %s", sourcePath, targetPath)

	err := os.MkdirAll(filepath.Dir(targetPath), os.ModePerm)
	if err != nil {
		return err
	}

	return file.Copy(sourcePath, targetPath)
}

// purpose
//   helper that checks if a given file exists or not.
//
// inputs:
//   'filePath' [in]:
//   - path of file to check.
//
// output:
//   true if the file exists, otherwise, false.
//
func fileExists(filePath string) (bool, error) {
	_, err := os.Stat(filePath)

	if err == nil {
		// File exists
		return true, nil
	} else if os.IsNotExist(err) {
		// File does not exist
		return false, nil
	} else {
		// An error occurred (other than file not existing)
		return false, err
	}
}