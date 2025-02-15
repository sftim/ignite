package operations

import (
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path"

	log "github.com/sirupsen/logrus"
	api "github.com/weaveworks/ignite/pkg/apis/ignite/v1alpha1"
	meta "github.com/weaveworks/ignite/pkg/apis/meta/v1alpha1"
	"github.com/weaveworks/ignite/pkg/client"
	"github.com/weaveworks/ignite/pkg/constants"
	"github.com/weaveworks/ignite/pkg/filter"
	"github.com/weaveworks/ignite/pkg/metadata/imgmd"
	"github.com/weaveworks/ignite/pkg/metadata/kernmd"
	"github.com/weaveworks/ignite/pkg/source"
	"github.com/weaveworks/ignite/pkg/storage/filterer"
	"github.com/weaveworks/ignite/pkg/util"
)

// FindOrImportImage returns an image based on the source string.
// If the image already exists, it is returned. If the image doesn't
// exist, it is imported
func FindOrImportImage(c *client.Client, ociRef meta.OCIImageRef) (*imgmd.Image, error) {
	log.Debugf("Ensuring image %s exists, or importing it...", ociRef)
	image, err := c.Images().Find(filter.NewIDNameFilter(ociRef.String()))
	if err == nil {
		// Return the image found
		log.Debugf("Found image with UID %s", image.GetUID())
		return imgmd.WrapImage(image), nil
	}

	switch err.(type) {
	case *filterer.NonexistentError:
		return importImage(c, ociRef)
	default:
		return nil, err
	}
}

// importKernel imports an image from an OCI image
func importImage(c *client.Client, ociRef meta.OCIImageRef) (*imgmd.Image, error) {
	log.Debugf("Importing image with ociRef %q", ociRef)
	// Parse the source
	dockerSource := source.NewDockerSource()
	src, err := dockerSource.Parse(ociRef)
	if err != nil {
		return nil, err
	}

	image := &api.Image{
		ObjectMeta: meta.ObjectMeta{
			Name: ociRef.String(),
		},
		Spec: api.ImageSpec{
			OCIClaim: api.OCIImageClaim{
				Ref: ociRef,
			},
		},
		Status: api.ImageStatus{
			OCISource: *src,
		},
	}

	// Create a new image runtime object
	runImage, err := imgmd.NewImage(image, c)
	if err != nil {
		return nil, err
	}

	log.Println("Starting image import...")

	// Create new file to host the filesystem and format it
	if err := runImage.AllocateAndFormat(); err != nil {
		return nil, err
	}

	// Add the files to the filesystem
	if err := runImage.AddFiles(dockerSource); err != nil {
		return nil, err
	}

	if err := runImage.Save(); err != nil {
		return nil, err
	}

	log.Printf("Imported OCI image %q (%s) to base image with UID %q", ociRef, runImage.Status.OCISource.Size, runImage.GetUID())
	return runImage, nil
}

// FindOrImportKernel returns an kernel based on the source string.
// If the image already exists, it is returned. If the image doesn't
// exist, it is imported
func FindOrImportKernel(c *client.Client, ociRef meta.OCIImageRef) (*kernmd.Kernel, error) {
	log.Debugf("Ensuring kernel %s exists, or importing it...", ociRef)
	kernel, err := c.Kernels().Find(filter.NewIDNameFilter(ociRef.String()))
	if err == nil {
		// Return the kernel found
		log.Debugf("Found kernel with UID %s", kernel.GetUID())
		return kernmd.WrapKernel(kernel), nil
	}

	switch err.(type) {
	case *filterer.NonexistentError:
		return importKernel(c, ociRef)
	default:
		return nil, err
	}
}

// importKernel imports a kernel from an OCI image
func importKernel(c *client.Client, ociRef meta.OCIImageRef) (*kernmd.Kernel, error) {
	log.Debugf("Importing kernel with ociRef %q", ociRef)
	// Parse the source
	dockerSource := source.NewDockerSource()
	src, err := dockerSource.Parse(ociRef)
	if err != nil {
		return nil, err
	}

	kernel := &api.Kernel{
		ObjectMeta: meta.ObjectMeta{
			Name: ociRef.String(),
		},
		Spec: api.KernelSpec{
			OCIClaim: api.OCIImageClaim{
				Ref: ociRef,
			},
		},
		Status: api.KernelStatus{
			OCISource: *src,
		},
	}

	// Create new kernel metadata
	runKernel, err := kernmd.NewKernel(kernel, c)
	if err != nil {
		return nil, err
	}

	// Cache the kernel contents in the kernel tar file
	kernelTarFile := path.Join(runKernel.ObjectPath(), constants.KERNEL_TAR)

	if !util.FileExists(kernelTarFile) {
		f, err := os.Create(kernelTarFile)
		if err != nil {
			return nil, err
		}
		defer f.Close()

		reader, err := dockerSource.Reader()
		if err != nil {
			return nil, err
		}
		defer reader.Close()

		// Copy over the contents from the OCI image into the tar file
		if _, err := io.Copy(f, reader); err != nil {
			return nil, err
		}

		// Remove the temporary container
		if err := dockerSource.Cleanup(); err != nil {
			return nil, err
		}
	}

	// vmlinuxFile describes the uncompressed kernel file at /var/lib/firecracker/kernel/$id/vmlinux
	vmlinuxFile := path.Join(runKernel.ObjectPath(), constants.KERNEL_FILE)
	// Create it if it doesn't exist
	if !util.FileExists(vmlinuxFile) {
		// Create a temporary directory for extracting the kernel file
		tempDir, err := ioutil.TempDir("", "")
		if err != nil {
			return nil, err
		}
		// Extract only the boot directory from the tar file cache to the temp dir
		if _, err := util.ExecuteCommand("tar", "-xf", kernelTarFile, "-C", tempDir, "boot"); err != nil {
			return nil, err
		}

		// Locate the kernel file in the temporary directory
		kernelTmpFile, err := findKernel(tempDir)
		if err != nil {
			return nil, err
		}

		// Copy the vmlinux file
		if err := util.CopyFile(kernelTmpFile, vmlinuxFile); err != nil {
			return nil, fmt.Errorf("failed to copy kernel file %q to kernel %q: %v", kernelTmpFile, runKernel.GetUID(), err)
		}

		// Cleanup
		if err := os.RemoveAll(tempDir); err != nil {
			return nil, err
		}
	}

	// Populate the kernel version field if possible
	if len(runKernel.Status.Version) == 0 {
		cmd := fmt.Sprintf(`strings %s | grep 'Linux version' | awk '{print $3}'`, vmlinuxFile)
		out, err := util.ExecuteCommand("/bin/bash", "-c", cmd)
		if err != nil {
			runKernel.Status.Version = "<unknown>"
		} else {
			runKernel.Status.Version = string(out)
		}
	}

	// Save the metadata
	if err := runKernel.Save(); err != nil {
		return nil, err
	}

	log.Printf("Imported OCI image %q (%s) to kernel image with UID %q", ociRef, runKernel.Status.OCISource.Size, runKernel.GetUID())
	return runKernel, nil
}

func findKernel(tmpDir string) (string, error) {
	// find the path to the kernel, resolve symlinks if necessary
	bootDir := path.Join(tmpDir, "boot")
	kernel := path.Join(bootDir, constants.KERNEL_FILE)

	fi, err := os.Lstat(kernel)
	if err != nil {
		return "", err
	}

	if fi.Mode()&os.ModeSymlink == 0 {
		// The target file is a real file, not a symlink. Return it
		return kernel, nil
	}

	// The target is a symlink
	kernel, err = os.Readlink(kernel)
	if err != nil {
		return "", err
	}

	// Cleanup the path for absolute and relative symlinks
	if path.IsAbs(kernel) {
		// return the path relative to the tempdir (root)
		// NOTE: This will fail if the symlink starts with any directory other than
		// "/boot", as we don't extract more
		return path.Join(tmpDir, kernel), nil
	}

	// Return the path relative to the boot directory
	return path.Join(bootDir, kernel), nil
}
