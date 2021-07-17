package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/avast/retry-go"
	"github.com/mitchellh/go-homedir"
	"github.com/sfreiberg/simplessh"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
	"k8s.io/klog/v2"
)

type cluster struct {
	Cpu     *int
	Memory  string
	KeyPath string
	Disk    string
	Name    string
	Network string
	Hosts   []*host
}

type host struct {
	Name        string
	Username    string
	Address     string
	RangeStart  string
	RangeEnd    string
	Gateway     string
	Controllers int
	Workers     int
	client      *simplessh.Client
	imagePath   string
	clusterPath string
	nodes       []*node
}

type node struct {
	instanceName string
	hostname     string
	role         string
	address      string
	index        int
	netplan      *netplan
}

const (
	DEFAULTNETWORK = "gomp"
)

func init() {
	createCmd.PersistentFlags().IntVarP(&worker, "worker", "w", 0, "worker")
	createCmd.PersistentFlags().IntVarP(&controller, "controller", "n", 1, "controller")
	createCmd.PersistentFlags().IntVarP(&cpu, "cpu", "c", 4, "cpu")
	createCmd.PersistentFlags().StringVarP(&file, "file", "f", "", "file")
	createCmd.PersistentFlags().StringVarP(&disk, "disk", "d", "20G", "disk")
	createCmd.PersistentFlags().StringVarP(&memory, "memory", "m", "16G", "memory")
	createCmd.PersistentFlags().StringVarP(&privateKeyPath, "privateKeyPath", "p", "~/.ssh/id_rsa", "privateKeyPath")
	createCmd.PersistentFlags().StringVarP(&keyPath, "keyPath", "k", "~/.ssh/id_rsa.pub", "privateKeyPath")
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
		if file != "" {
			klog.Info("creating cluster")
			if err := createCluster(); err != nil {
				klog.Error(err)
				os.Exit(0)
			}
		} else if len(args) < 1 {
			klog.Errorf("missing name")
			os.Exit(0)
		} else if err := create(args[0]); err != nil {
			klog.Error(err)
		}
	},
}

func createCluster() error {
	clusterByte, err := ioutil.ReadFile(file)
	if err != nil {
		return err
	}
	cl := &cluster{}
	if err := yaml.Unmarshal(clusterByte, cl); err != nil {
		return err
	}
	if cl.Cpu == nil {
		cl.Cpu = &cpu
	}
	if cl.Memory == "" {
		cl.Memory = memory
	}
	if cl.KeyPath == "" {
		cl.KeyPath = privateKeyPath
	}
	if cl.Disk == "" {
		cl.Disk = disk
	}
	if cl.Network == "" {
		cl.Network = DEFAULTNETWORK
	}
	controllerCounter := 0
	workerCounter := 0
	totalCounter := 0
	for _, h := range cl.Hosts {
		var nodeList []*node
		nodeCounter := 0
		for i := 0; i < h.Controllers; i++ {
			n := &node{
				instanceName: fmt.Sprintf("%sc%d--%s", h.Name, controllerCounter, cl.Name),
				hostname:     fmt.Sprintf("%sc%d.%s.%s", h.Name, controllerCounter, cl.Name, suffix),
				role:         "controller",
				index:        totalCounter,
				netplan:      getNetplan(totalCounter+1, nodeCounter+1, h.Gateway),
			}
			nodeCounter++
			controllerCounter++
			totalCounter++
			nodeList = append(nodeList, n)
		}
		for i := 0; i < h.Workers; i++ {
			n := &node{
				instanceName: fmt.Sprintf("%sx%d--%s", h.Name, workerCounter, cl.Name),
				hostname:     fmt.Sprintf("%sx%d.%s.%s", h.Name, workerCounter, cl.Name, suffix),
				role:         "worker",
				index:        totalCounter,
				netplan:      getNetplan(totalCounter+1, nodeCounter+1, h.Gateway),
			}
			nodeCounter++
			workerCounter++
			totalCounter++
			nodeList = append(nodeList, n)
		}
		h.nodes = nodeList
	}
	var hostWG sync.WaitGroup
	for _, h := range cl.Hosts {
		hostWG.Add(1)
		go h.process(cl, &hostWG)
	}
	hostWG.Wait()
	if err := cl.inventory(suffix); err != nil {
		return err
	}
	return nil
}

func getNetplan(totalIndex int, hostIndex int, gateway string) *netplan {
	mac := "de:ad:be:ef:ba:01"
	mac = strings.ReplaceAll(mac, ":", "")
	result, err := strconv.ParseInt(mac, 16, 0)
	if err != nil {
		panic(err)
	}
	mac = fmt.Sprintf("%x", result+int64(totalIndex))
	mac = insertNth(mac, 2)
	ip, _, _ := net.ParseCIDR(gateway)
	nIP := nextIP(ip, uint(hostIndex))
	gwList := strings.Split(gateway, "/")
	np := &netplan{
		mac: mac,
		gw:  gwList[0],
		ip:  nIP.String() + "/" + gwList[1],
	}
	return np
}

func (c *cluster) inventory(suffix string) error {
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
	clusterPath := filepath.Join(gompPath, c.Name)
	if _, err := os.Stat(clusterPath); os.IsNotExist(err) {
		if err := os.Mkdir(clusterPath, 0755); err != nil {
			return err
		}
	}
	var allHosts = make(map[string]Host)
	var kubeMasterHosts = make(map[string]struct{})
	var kubeNodeHosts = make(map[string]struct{})
	var etcdHosts = make(map[string]struct{})

	for _, h := range c.Hosts {
		for _, n := range h.nodes {
			allHosts[n.hostname] = Host{
				AnsibleHost: n.address,
				IP:          n.address,
			}
			switch n.role {
			case "controller":
				kubeMasterHosts[n.hostname] = struct{}{}
				etcdHosts[n.hostname] = struct{}{}

			case "worker":
				kubeNodeHosts[n.hostname] = struct{}{}
			}
		}
	}

	i := Inventory{
		All: All{
			Hosts: allHosts,
			Vars: map[string]string{
				"enable_nodelocaldns":        "false",
				"download_run_once":          "true",
				"download_localhost":         "true",
				"enable_dual_stack_networks": "true",
				"ansible_user":               "root",
				"docker_image_repo":          "svl-artifactory.juniper.net/atom-docker-remote",
				"cluster_name":               fmt.Sprintf("%s.%s", c.Name, suffix),
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
	inventoryString = regexp.MustCompile(`"(true|false)"`).ReplaceAllString(inventoryString, `$1`)
	if err := os.WriteFile(clusterPath+"/inventory.yaml", []byte(inventoryString), 0600); err != nil {
		return err
	}
	klog.Infof("created inventory file %s/inventory.yaml", clusterPath)
	return nil
}

func (h *host) process(cl *cluster, hostWG *sync.WaitGroup) {
	defer hostWG.Done()
	if err := h.prepare(cl); err != nil {
		klog.Error(err)
	}
	mpList, err := h.remoteListMultipassInstances()
	if err != nil {
		klog.Error(err)
	}

	var wg sync.WaitGroup
	for _, n := range h.nodes {
		if !instanceExists(n.instanceName, mpList) {
			wg.Add(1)
			go h.remoteCreateInstance(cl, n.hostname, n.instanceName, h.clusterPath, h.imagePath, n.netplan, &wg)
			time.Sleep(time.Second * 2)
		} else {
			klog.Infof("instance %s already exists. Skipping", n.instanceName)
		}
	}

	wg.Wait()
	allIPSfound := false
	for !allIPSfound {
		newMPList, err := h.remoteListMultipassInstances()
		if err != nil {
			klog.Error(err)
		}
		for _, n := range h.nodes {
			for _, mp := range newMPList.List {
				if mp.Name == n.instanceName {
					if len(mp.Ipv4) > 0 {
						for _, ipaddress := range mp.Ipv4 {
							_, ipnet, _ := net.ParseCIDR(h.Gateway)
							if ipnet.Contains(net.ParseIP(ipaddress)) {
								n.address = ipaddress
								if err := retry.Do(
									func() error {
										return keyScan(ipaddress)
									},
									retry.Attempts(10),
									retry.Delay(time.Second*2),
								); err != nil {
									klog.Error(err)
								}
								allIPSfound = h.allIPS()
							}
						}
					}
				}
			}
		}
		klog.Infof("not all ips available, waiting")
		time.Sleep(time.Second * 2)
	}
}

func (h *host) allIPS() bool {
	numIPS := 0
	for _, n := range h.nodes {
		if n.address != "" {
			numIPS++
		}
	}
	return numIPS == len(h.nodes)
}

func (h *host) prepare(cl *cluster) error {
	klog.Infof("prapring host %s", h.Address)
	if h.Username == "" {
		currentUser, err := user.Current()
		if err != nil {
			return err
		}
		h.Username = currentUser.Username
	}
	expandedKeyPath, err := homedir.Expand(cl.KeyPath)
	if err != nil {
		return err
	}
	client, err := sshClient(h.Address, h.Username, expandedKeyPath)
	if err != nil {
		return err
	}
	h.client = client
	home, err := client.Exec("echo $HOME")
	if err != nil {
		return err
	}
	homepath := strings.TrimSuffix(string(home), "\n")
	gompPath := filepath.Join(homepath, ".gomp")
	clusterPath := filepath.Join(gompPath, cl.Name)
	clusterPathExists, err := client.Exec(fmt.Sprintf("ls %s", clusterPath))
	if err != nil {
		if strings.TrimSuffix(string(clusterPathExists), "\n") == fmt.Sprintf("ls: cannot access '%s': No such file or directory", clusterPath) {
			if _, err := client.Exec(fmt.Sprintf("mkdir -p %s", clusterPath)); err != nil {
				return err
			}
		} else {
			return err
		}
	}
	imageName := filepath.Base(imageURL)
	imagePath := filepath.Join(gompPath, imageName)
	imagePathExists, err := client.Exec(fmt.Sprintf("ls %s", imagePath))
	if err != nil {
		if strings.TrimSuffix(string(imagePathExists), "\n") == fmt.Sprintf("ls: cannot access '%s': No such file or directory", imagePath) {
			klog.Infof("image %s not found on host %s, downloading it from %s", imageName, h.Name, imageURL)
			if _, err := client.Exec(fmt.Sprintf("curl -o %s %s", imagePath, imageURL)); err != nil {
				return err
			}
		} else {
			return err
		}
	}
	h.imagePath = imagePath
	h.clusterPath = clusterPath
	return nil
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
		return fmt.Errorf(err.Error(), string(key))
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
				//"dns_mode":                   "coredns",
				//"nodelocaldns_ip":            "10.96.0.10",
				"enable_nodelocaldns":        "false",
				"download_run_once":          "true",
				"download_localhost":         "true",
				"enable_dual_stack_networks": "true",
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
	inventoryString = regexp.MustCompile(`"(true|false)"`).ReplaceAllString(inventoryString, `$1`)
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
	cloudInitPath, err := createCloudInit(hostname, string(keyByte), path, nil)
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

func (h *host) remoteCreateInstance(cl *cluster, hostname, instanceName, path, imagePath string, np *netplan, wg *sync.WaitGroup) {
	defer wg.Done()
	expandedKeyPath, err := homedir.Expand(keyPath)
	if err != nil {
		klog.Error(err)
	}
	keyByte, err := os.ReadFile(expandedKeyPath)
	if err != nil {
		klog.Error(err)
	}
	f, err := ioutil.TempDir("/tmp", "cloudinit")
	if err != nil {
		klog.Error(err)
	}
	defer os.Remove(f)
	cloudInitPath, err := createCloudInit(hostname, string(keyByte), f, np)
	if err != nil {
		klog.Error(err)
	}
	cloudInitByte, err := os.ReadFile(cloudInitPath)
	if err != nil {
		klog.Error(err)
	}
	execOut, err := h.client.Exec(fmt.Sprintf("echo \"%s\" > %s/%s", string(cloudInitByte), h.clusterPath, filepath.Base(cloudInitPath)))
	if err != nil {
		klog.Error(err, string(execOut))
	}
	cmdString := fmt.Sprintf("multipass launch -c %d -d %s -m %s -n %s --network name=%s,mode=manual,mac=%s --cloud-init %s/%s file://%s", *cl.Cpu, cl.Disk, cl.Memory, instanceName, cl.Network, np.mac, h.clusterPath, filepath.Base(cloudInitPath), imagePath)
	klog.Infof("creating instance %s on host %s", instanceName, h.Name)
	out, err := h.client.Exec(cmdString)
	if err != nil {
		klog.Error("ERROR: ", err, string(out), cmdString)
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

func (h *host) remoteListMultipassInstances() (*MultipassList, error) {
	h.client.Exec("multipass list --format json")
	out, err := h.client.Exec("multipass list --format json")
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

type netplan struct {
	mac string
	ip  string
	gw  string
}

func createCloudInit(hostname, key, out string, np *netplan) (string, error) {

	ci := cloudInit{
		Hostname:       hostname,
		ManageEtcHosts: true,
		Users: []instanceUser{{
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

	if np != nil {
		netplanString := fmt.Sprintf(`network:
    ethernets:
        extra0:
            dhcp4: false
            match:
                macaddress: %s
            addresses:
                - %s
            gateway4: %s
    version: 2`, np.mac, np.ip, np.gw)
		ci.WriteFiles = append(ci.WriteFiles, writeFiles{
			Content: netplanString,
			Path:    "/etc/netplan/intf.yaml",
		})
		ci.RunCMD = append(ci.RunCMD, "netplan apply")
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
	Users          []instanceUser    `yaml:"users"`
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

type instanceUser struct {
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
	IP          string `yaml:"ip"`
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

func sshClient(host, username, key string) (*simplessh.Client, error) {
	client, err := simplessh.ConnectWithKeyFile(host, username, key)
	if err != nil {
		return nil, err
	}
	return client, nil
}

func insertNth(s string, n int) string {
	var buffer bytes.Buffer
	var n_1 = n - 1
	var l_1 = len(s) - 1
	for i, rune := range s {
		buffer.WriteRune(rune)
		if i%n == n_1 && i != l_1 {
			buffer.WriteRune(':')
		}
	}
	return buffer.String()
}

func nextIP(ip net.IP, inc uint) net.IP {
	i := ip.To4()
	v := uint(i[0])<<24 + uint(i[1])<<16 + uint(i[2])<<8 + uint(i[3])
	v += inc
	v3 := byte(v & 0xFF)
	v2 := byte((v >> 8) & 0xFF)
	v1 := byte((v >> 16) & 0xFF)
	v0 := byte((v >> 24) & 0xFF)
	return net.IPv4(v0, v1, v2, v3)
}
