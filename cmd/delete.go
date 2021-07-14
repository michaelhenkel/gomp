package cmd

import (
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/spf13/cobra"
	"k8s.io/klog/v2"
)

var instance string

func init() {
	deleteCmd.PersistentFlags().StringVarP(&instance, "instance", "i", "", "instance")
}

var deleteCmd = &cobra.Command{
	Use:   "delete",
	Short: "",
	Long:  ``,
	Run: func(cmd *cobra.Command, args []string) {
		if len(args) < 1 {
			klog.Errorf("missing name")
			os.Exit(0)
		}
		if err := delete(args[0]); err != nil {
			klog.Error(err)
		}
	},
}

func delete(clusterName string) error {
	mpList, err := listMultipassInstances()
	if err != nil {
		return err
	}
	var wg sync.WaitGroup
	for _, mp := range mpList.List {
		clusterList := strings.Split(mp.Name, "--")
		if len(clusterList) > 1 {
			if clusterList[1] == clusterName {
				if instance == "" {
					wg.Add(1)
					go deleteInstance(mp.Name, &wg)
				} else {
					if instance == clusterList[0] {
						wg.Add(1)
						go deleteInstance(mp.Name, &wg)
					}
				}
			}
		}
	}
	wg.Wait()
	//time.Sleep(time.Second * 3)
	_, err = exec.Command("multipass", "purge").Output()
	if err != nil {
		return err
	}
	return nil
}

func deleteInstance(name string, wg *sync.WaitGroup) error {
	defer wg.Done()
	_, err := exec.Command("multipass", "delete", name).Output()
	if err != nil {
		return err
	}
	return nil
}
