package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/mitchellh/go-homedir"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
	"k8s.io/klog/v2"
)

func init() {
	createCmd.PersistentFlags().IntVarP(&worker, "worker", "w", 0, "worker")
	createCmd.PersistentFlags().IntVarP(&controller, "controller", "n", 1, "controller")
	createCmd.PersistentFlags().IntVarP(&cpu, "cpu", "c", 4, "cpu")
	createCmd.PersistentFlags().StringVarP(&disk, "disk", "d", "20G", "disk")
	createCmd.PersistentFlags().StringVarP(&memory, "memory", "m", "16G", "memory")
	createCmd.PersistentFlags().StringVarP(&keyPath, "keyPath", "k", "~/.ssh/id_rsa.pub", "keyPath")
}

var (
	suffix   = "local"
	imageURL = "https://cloud-images.ubuntu.com/releases/focal/release-20210315/ubuntu-20.04-server-cloudimg-amd64.img"
)

var createCmd = &cobra.Command{
	Use:   "create",
	Short: "",
	Long:  ``,
	Run: func(cmd *cobra.Command, args []string) {
		if len(args) < 1 {
			klog.Errorf("missing name")
			os.Exit(0)
		}
		if err := create(args[0]); err != nil {
			klog.Error(err)
		}
	},
}

func create(clusterName string) error {
	hd, err := homedir.Dir()
	if err != nil {
		return err
	}
	gompPath := filepath.Join(hd, ".gomp")
	if _, err := os.Stat(gompPath); os.IsNotExist(err) {
		if err := os.Mkdir(gompPath, 0755); err != nil {
			return err
		}
	}
	clusterPath := filepath.Join(gompPath, clusterName)
	if _, err := os.Stat(clusterPath); os.IsNotExist(err) {
		if err := os.Mkdir(clusterPath, 0755); err != nil {
			return err
		}
	}
	imageName := filepath.Base(imageURL)
	imagePath := filepath.Join(gompPath, imageName)
	if _, err := os.Stat(imagePath); os.IsNotExist(err) {
		if _, err := os.Stat(imagePath); os.IsNotExist(err) {
			out, err := os.Create(imagePath)
			if err != nil {
				return err
			}
			defer out.Close()
			resp, err := http.Get(imageURL)
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			_, err = io.Copy(out, resp.Body)
			if err != nil {
				return err
			}
		}
	}
	mpList, err := listMultipassInstances()
	if err != nil {
		return err
	}
	var wg sync.WaitGroup
	for i := 0; i < controller; i++ {
		instanceName := fmt.Sprintf("c%d--%s", i, clusterName)
		hostname := fmt.Sprintf("c%d.%s.%s", i, clusterName, suffix)
		if !instanceExists(instanceName, mpList) {
			wg.Add(1)
			go createInstance(hostname, instanceName, clusterPath, imagePath, &wg)
			time.Sleep(time.Second * 2)
		} else {
			klog.Infof("instance %s already exists. Skipping", instanceName)
		}
	}
	for i := 0; i < worker; i++ {
		instanceName := fmt.Sprintf("x%d--%s", i, clusterName)
		hostname := fmt.Sprintf("x%d.%s.%s", i, clusterName, suffix)
		if !instanceExists(instanceName, mpList) {
			wg.Add(1)
			go createInstance(hostname, instanceName, clusterPath, imagePath, &wg)
			time.Sleep(time.Second * 2)
		} else {
			klog.Infof("instance %s already exists. Skipping", instanceName)
		}
	}
	wg.Wait()
	var instanceMap = make(map[string]instanceIPRole)
	newMPList, err := listMultipassInstances()
	if err != nil {
		return err
	}
	for i := 0; i < controller; i++ {
		instanceName := fmt.Sprintf("c%d--%s", i, clusterName)
		hostname := fmt.Sprintf("c%d.%s.%s", i, clusterName, suffix)
		for _, mp := range newMPList.List {
			if mp.Name == instanceName {
				instanceMap[hostname] = instanceIPRole{role: "controller", ip: mp.Ipv4[0]}
				if err := keyScan(mp.Ipv4[0]); err != nil {
					return err
				}
			}
		}
	}
	for i := 0; i < worker; i++ {
		instanceName := fmt.Sprintf("x%d--%s", i, clusterName)
		hostname := fmt.Sprintf("x%d.%s.%s", i, clusterName, suffix)
		for _, mp := range newMPList.List {
			if mp.Name == instanceName {
				instanceMap[hostname] = instanceIPRole{role: "worker", ip: mp.Ipv4[0]}
				if err := keyScan(mp.Ipv4[0]); err != nil {
					return err
				}
			}
		}
	}
	if err := inventory(instanceMap, clusterName, suffix, clusterPath); err != nil {
		return err
	}
	return nil
}

func keyScan(ip string) error {
	knowHosts, err := homedir.Expand("~/.ssh/known_hosts")
	if err != nil {
		return err
	}
	klog.Infof("adding ip %s to %s", ip, knowHosts)
	key, err := exec.Command("ssh-keyscan", "-H", ip).Output()
	if err != nil {
		return err
	}
	f, err := os.OpenFile(knowHosts, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	if _, err := f.Write(key); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return nil
}

func inventory(instanceMap map[string]instanceIPRole, clusterName, suffix, clusterPath string) error {
	var allHosts = make(map[string]Host)
	var kubeMasterHosts = make(map[string]struct{})
	var kubeNodeHosts = make(map[string]struct{})
	var etcdHosts = make(map[string]struct{})

	for instName, inst := range instanceMap {
		allHosts[instName] = Host{
			AnsibleHost: inst.ip,
		}
		switch inst.role {
		case "controller":
			kubeMasterHosts[instName] = struct{}{}
			etcdHosts[instName] = struct{}{}

		case "worker":
			kubeNodeHosts[instName] = struct{}{}
		}

	}
	i := Inventory{
		All: All{
			Hosts: allHosts,
			Vars: map[string]string{
				"enable_dual_stack_networks": "true",
				"enable_nodelocaldns":        "false",
				"ansible_user":               "root",
				"docker_image_repo":          "svl-artifactory.juniper.net/atom-docker-remote",
				"cluster_name":               fmt.Sprintf("%s.%s", clusterName, suffix),
				"artifacts_dir":              clusterPath,
				"kube_network_plugin":        "cni",
				"kube_network_plugin_multus": "false",
				"kubectl_localhost":          "true",
				"kubeconfig_localhost":       "true",
				"override_system_hostname":   "true",
				"container_manager":          "crio",
				"kubelet_deployment_type":    "host",
				"download_container":         "false",
				"etcd_deployment_type":       "host",
				"host_key_checking":          "false",
			},
		},
		KubeMaster: KubeMaster{
			Hosts: kubeMasterHosts,
		},
		KubeNode: KubeNode{
			Hosts: kubeNodeHosts,
		},
		Etcd: Etcd{
			Hosts: etcdHosts,
		},
		K8SCluster: K8SCluster{
			Children: map[string]struct{}{
				"kube-master": struct{}{},
				"kube-node":   struct{}{},
			},
		},
	}
	inventoryByte, err := yaml.Marshal(&i)
	if err != nil {
		return err
	}
	inventoryString := strings.Replace(string(inventoryByte), "{}", "", -1)
	if err := os.WriteFile(clusterPath+"/inventory.yaml", []byte(inventoryString), 0600); err != nil {
		return err
	}
	klog.Infof("created inventory file %s/inventory.yaml", clusterPath)
	return nil
}

type instanceIPRole struct {
	role string
	ip   string
}

func instanceExists(instanceName string, mpList *MultipassList) bool {
	for _, mp := range mpList.List {
		if mp.Name == instanceName {
			return true
		}
	}
	return false
}

func createInstance(hostname, instanceName, path, imagePath string, wg *sync.WaitGroup) {
	defer wg.Done()
	expandedKeyPath, err := homedir.Expand(keyPath)
	if err != nil {
		klog.Fatal(err)
	}
	keyByte, err := os.ReadFile(expandedKeyPath)
	if err != nil {
		klog.Fatal(err)
	}
	cloudInitPath, err := createCloudInit(hostname, string(keyByte), path)
	if err != nil {
		klog.Fatal(err)
	}
	cmdString := fmt.Sprintf("multipass launch -c %d -d %s -m %s -n %s --cloud-init %s file://%s", cpu, disk, memory, instanceName, cloudInitPath, imagePath)
	cmdList := strings.Split(cmdString, " ")
	klog.Infof("creating instance %s", instanceName)

	out, err := exec.Command(cmdList[0], cmdList[1:]...).CombinedOutput()
	if err != nil {
		klog.Errorln("ERROR", err, string(out), cmdString)
	}
}

func listMultipassInstances() (*MultipassList, error) {
	out, err := exec.Command("multipass", "list", "--format", "json").Output()
	if err != nil {
		return nil, err
	}
	mp := &MultipassList{}
	if err := json.Unmarshal(out, mp); err != nil {
		return nil, err
	}
	return mp, nil
}

func getMultipassInstance(instanceName string) (*Multipass, error) {
	mpList, err := listMultipassInstances()
	if err != nil {
		return nil, err
	}
	for _, mp := range mpList.List {
		if mp.Name == instanceName {
			return mp, nil
		}
	}
	return nil, nil
}

func createCloudInit(hostname, key, out string) (string, error) {
	ci := cloudInit{
		Hostname:       hostname,
		ManageEtcHosts: true,
		Users: []user{{
			Name:              "contrail",
			Sudo:              "ALL=(ALL) NOPASSWD:ALL",
			Home:              "/home/contrail",
			Shell:             "/bin/bash",
			LockPasswd:        false,
			SSHAuthorizedKeys: []string{key},
		}, {
			Name:              "root",
			Sudo:              "ALL=(ALL) NOPASSWD:ALL",
			SSHAuthorizedKeys: []string{key},
		}},
		SSHPwauth:   true,
		DisableRoot: false,
		Chpasswd: chpasswd{
			List: `contrail:contrail
root:gokvm`,
			Expire: false,
		},
		WriteFiles: []writeFiles{{
			Content: `[Resolve]
DNS=172.29.131.60`,
			Path: "/etc/systemd/resolved.conf",
		}},
		RunCMD: []string{
			"systemctl restart systemd-resolved.service",
		},
	}
	ciByte, err := yaml.Marshal(&ci)
	if err != nil {
		return "", err
	}
	cloudInitPath := filepath.Join(out, hostname+".cloudinit")
	if err := os.WriteFile(cloudInitPath, ciByte, 0600); err != nil {
		return "", err
	}
	return cloudInitPath, nil
}

type cloudInit struct {
	Hostname       string            `yaml:"hostname"`
	ManageEtcHosts bool              `yaml:"manage_etc_hosts"`
	Users          []user            `yaml:"users"`
	SSHPwauth      bool              `yaml:"ssh_pwauth"`
	DisableRoot    bool              `yaml:"disable_root"`
	Chpasswd       chpasswd          `yaml:"chpasswd"`
	WriteFiles     []writeFiles      `yaml:"write_files"`
	RunCMD         []string          `yaml:"runcmd"`
	APT            map[string]source `yaml:"apt"`
	Snap           map[string]string `yaml:"snap"`
	Network        netw              `yaml:"network"`
}

type netw struct {
	Version   string              `yaml:"version"`
	Ethernets map[string]ethernet `yaml:"ethernets"`
}

type ethernet struct {
	Match map[string]string `yaml:"match"`
	Dhcp4 bool              `yaml:"dhcp4"`
}

type source struct {
	Source string `yaml:"source"`
	KeyID  string `yaml:"keyid"`
}

type chpasswd struct {
	List   string `yaml:"list"`
	Expire bool   `yaml:"expire"`
}

type writeFiles struct {
	Content string `yaml:"content"`
	Path    string `yaml:"path"`
}

type user struct {
	Name              string   `yaml:"name"`
	Sudo              string   `yaml:"sudo"`
	Groups            string   `yaml:"groups"`
	Home              string   `yaml:"home"`
	Shell             string   `yaml:"shell"`
	LockPasswd        bool     `yaml:"lock_passwd"`
	SSHAuthorizedKeys []string `yaml:"ssh-authorized-keys"`
}

type MultipassList struct {
	List []*Multipass `json:"list"`
}

type Multipass struct {
	Ipv4    []string `json:"ipv4"`
	Name    string   `json:"name"`
	Release string   `json:"release"`
	State   string   `json:"state"`
}

type Host struct {
	AnsibleHost string `yaml:"ansible_host"`
}

type All struct {
	Hosts map[string]Host   `yaml:"hosts"`
	Vars  map[string]string `yaml:"vars"`
}

type KubeMaster struct {
	Hosts map[string]struct{}
}

type KubeNode struct {
	Hosts map[string]struct{}
}

type Etcd struct {
	Hosts map[string]struct{}
}

type K8SCluster struct {
	Children map[string]struct{} `yaml:"children"`
}

type Inventory struct {
	All        All        `yaml:"all"`
	KubeMaster KubeMaster `yaml:"kube-master"`
	KubeNode   KubeNode   `yaml:"kube-node"`
	Etcd       Etcd       `yaml:"etcd"`
	K8SCluster K8SCluster `yaml:"k8s-cluster"`
}
