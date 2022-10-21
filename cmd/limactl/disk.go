package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"text/tabwriter"

	"github.com/docker/go-units"
	"github.com/lima-vm/lima/pkg/qemu"
	"github.com/lima-vm/lima/pkg/store"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

func newDiskCommand() *cobra.Command {
	var diskCommand = &cobra.Command{
		Use:   "disk",
		Short: "Lima disk management",
		Example: `  Create a disk:
  $ limactl disk create DISK

  List existing disks:
  $ limactl disk ls

  Delete a disk:
  $ limactl disk delete DISK`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	diskCommand.AddCommand(
		newDiskCreateCommand(),
		newDiskListCommand(),
		newDiskDeleteCommand(),
		newDiskUnlockCommand(),
	)
	return diskCommand
}

func newDiskCreateCommand() *cobra.Command {
	var diskCreateCommand = &cobra.Command{
		Use: "create DISK",
		Example: `
To create a new disk:
$ limactl disk create DISK
`,
		Short: "Create a Lima disk",
		Args:  cobra.ExactArgs(1),
		RunE:  diskCreateAction,
	}
	diskCreateCommand.Flags().String("size", "50G", "configure the disk size")
	return diskCreateCommand
}

func diskCreateAction(cmd *cobra.Command, args []string) error {
	// only exactly one arg is allowed
	name := args[0]

	diskDir, err := store.DiskDir(name)
	if err != nil {
		return err
	}

	if _, err := os.Stat(diskDir); !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("disk %q already exists (%q)", name, diskDir)
	}

	logrus.Infof("Creating a disk %q", name)

	if err := os.MkdirAll(diskDir, 0700); err != nil {
		return err
	}

	size, err := cmd.Flags().GetString("size")
	if err != nil {
		return err
	}
	diskSize, err := units.RAMInBytes(size)
	if err != nil {
		return err
	}

	if err := qemu.CreateDataDisk(diskDir, int(diskSize)); err != nil {
		return err
	}

	return nil
}

func newDiskListCommand() *cobra.Command {
	var diskListCommand = &cobra.Command{
		Use: "list",
		Example: `
To list existing disks:
$ limactl disk list
`,
		Short:   "List existing Lima disks",
		Aliases: []string{"ls"},
		Args:    cobra.NoArgs,
		RunE:    diskListAction,
	}
	diskListCommand.Flags().Bool("json", false, "JSONify output")
	return diskListCommand
}

func diskListAction(cmd *cobra.Command, args []string) error {
	jsonFormat, err := cmd.Flags().GetBool("json")
	if err != nil {
		return err
	}

	allDisks, err := store.Disks()
	if err != nil {
		return err
	}

	if jsonFormat {
		for _, diskName := range allDisks {
			disk, err := store.InspectDisk(diskName)
			if err != nil {
				logrus.WithError(err).Errorf("disk %q does not exist?", diskName)
				continue
			}
			j, err := json.Marshal(disk)
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), string(j))
		}
		return nil
	}

	w := tabwriter.NewWriter(cmd.OutOrStdout(), 4, 8, 4, ' ', 0)
	fmt.Fprintln(w, "NAME\tSIZE\tDIR\tIN USE BY")

	if len(allDisks) == 0 {
		logrus.Warn("No disk found. Run `limactl disk create DISK` to create a disk.")
	}

	for _, diskName := range allDisks {
		disk, err := store.InspectDisk(diskName)
		if err != nil {
			logrus.WithError(err).Errorf("disk %q does not exist?", diskName)
			continue
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", disk.Name, units.BytesSize(float64(disk.Size)), disk.Dir, disk.Instance)
	}

	return w.Flush()
}

func newDiskDeleteCommand() *cobra.Command {
	var diskDeleteCommand = &cobra.Command{
		Use: "delete DISK [DISK, ...]",
		Example: `
To delete a disk:
$ limactl disk delete DISK

To delete multiple disks:
$ limactl disk delete DISK1 DISK2 ...
`,
		Aliases: []string{"remove", "rm"},
		Short:   "Delete one or more Lima disks",
		Args:    cobra.MinimumNArgs(1),
		RunE:    diskDeleteAction,
	}
	diskDeleteCommand.Flags().Bool("force", false, "force delete")
	return diskDeleteCommand
}

func diskDeleteAction(cmd *cobra.Command, args []string) error {
	force, err := cmd.Flags().GetBool("force")
	if err != nil {
		return err
	}

	for _, diskName := range args {
		if force {
			disk, err := store.InspectDisk(diskName)
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					logrus.Warnf("Ignoring non-existent disk %q", diskName)
					continue
				}
				return err
			}

			if err := deleteDisk(disk); err != nil {
				return fmt.Errorf("failed to delete disk %q: %w", diskName, err)
			}
			logrus.Infof("Deleted %q (%q)", diskName, disk.Dir)
			continue
		}

		disk, err := store.InspectDisk(diskName)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				logrus.Warnf("Ignoring non-existent disk %q", diskName)
				continue
			}
			return err
		}
		if disk.Instance != "" {
			return fmt.Errorf("cannot delete disk %q in use by instance %q", disk.Name, disk.Instance)
		}
		instances, err := store.Instances()
		if err != nil {
			return err
		}
		var refInstances []string
		for _, instName := range instances {
			inst, err := store.Inspect(instName)
			if err != nil {
				continue
			}
			if len(inst.AdditionalDisks) > 0 {
				for _, d := range inst.AdditionalDisks {
					if d == diskName {
						refInstances = append(refInstances, instName)
					}
				}
			}
		}
		if len(refInstances) > 0 {
			logrus.Warnf("Skipping deleting disk %q, disk is referenced by one or more non-running instances: %q",
				diskName, refInstances)
			logrus.Warnf("To delete anyway, run %q", forceDeleteCommand(diskName))
			continue
		}
		if err := deleteDisk(disk); err != nil {
			return fmt.Errorf("failed to delete disk %q: %v", diskName, err)
		}
		logrus.Infof("Deleted %q (%q)", diskName, disk.Dir)
	}
	return nil
}

func deleteDisk(disk *store.Disk) error {
	if err := os.RemoveAll(disk.Dir); err != nil {
		return fmt.Errorf("failed to remove %q: %w", disk.Dir, err)
	}
	return nil
}

func forceDeleteCommand(diskName string) string {
	return fmt.Sprintf("limactl disk delete --force %v", diskName)
}

func newDiskUnlockCommand() *cobra.Command {
	var diskUnlockCommand = &cobra.Command{
		Use: "unlock DISK [DISK, ...]",
		Example: `
Emergency recovery! If an instance is force stopped, it may leave a disk locked while not actually using it.

To unlock a disk:
$ limactl disk unlock DISK

To unlock multiple disks:
$ limactl disk unlock DISK1 DISK2 ...
`,
		Short: "Unlock one or more Lima disks",
		Args:  cobra.MinimumNArgs(1),
		RunE:  diskUnlockAction,
	}
	return diskUnlockCommand
}

func diskUnlockAction(cmd *cobra.Command, args []string) error {
	for _, diskName := range args {
		disk, err := store.InspectDisk(diskName)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				logrus.Warnf("Ignoring non-existent disk %q", diskName)
				continue
			}
			return err
		}
		if disk.Instance == "" {
			logrus.Warnf("Ignoring unlocked disk %q", diskName)
			continue
		}
		// if store.Inspect throws an error, the instance does not exist, and it is safe to unlock
		inst, err := store.Inspect(disk.Instance)
		if err == nil {
			if len(inst.Errors) > 0 {
				logrus.Warnf("Cannot unlock disk %q, attached instance %q has errors: %+v",
					diskName, disk.Instance, inst.Errors)
				continue
			}
			if inst.Status == store.StatusRunning {
				logrus.Warnf("Cannot unlock disk %q used by running instance %q", diskName, disk.Instance)
				continue
			}
		}
		if err := disk.Unlock(); err != nil {
			return fmt.Errorf("failed to unlock disk %q: %w", diskName, err)
		}
		logrus.Infof("Unlocked disk %q (%q)", diskName, disk.Dir)
	}
	return nil
}
