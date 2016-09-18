package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/dustinkirkland/golang-petname"
	"github.com/pborman/uuid"
	"github.com/lxc/lxd"
	"github.com/lxc/lxd/shared"
)



func restReturnExistingContainer(uuid string, userId string, containerBaseName string, w http.ResponseWriter) {
	body := make(map[string]interface{})

	var containerName string
	var containerIP string
	var containerUsername string
	var containerPassword string
	var containerExpiry int64

	// get container data
	containerName, containerIP, containerUsername, containerPassword, containerExpiry, err := dbGetContainer(uuid)
	if err != nil || containerName == "" {
		fmt.Println("Error: ", containerName, "  ", containerIP)
		http.Error(w, "Container not found", 404)
		return
	}

	if !config.ServerConsoleOnly {
		body["ip"] = containerIP
		body["username"] = containerUsername
		body["password"] = containerPassword
		body["fqdn"] = fmt.Sprintf("%s.lxd", containerName)
	}
	body["id"] = uuid
	body["expiry"] = containerExpiry

	// Return to the client
	body["status"] = containerStarted
	err = json.NewEncoder(w).Encode(body)
	if err != nil {
		lxdForceDelete(lxdDaemon, containerName)
		http.Error(w, "Internal server error", 500)
		return
	}
}


func restCreateContainer(userId string, containerBaseName string, w http.ResponseWriter, requestIP string) {
	body := make(map[string]interface{})

	requestDate := time.Now().Unix()

	// Create the container
	//containerName := fmt.Sprintf("tryit-%s", petname.Adjective())
	containerName := fmt.Sprintf("%s%s", containerBaseName, userId)
	containerUsername := petname.Adjective()
	containerPassword := petname.Adjective()
	id := uuid.NewRandom().String()

	// Config
	ctConfig := map[string]string{}

	ctConfig["security.nesting"] = "true"
	if config.QuotaCPU > 0 {
		ctConfig["limits.cpu"] = fmt.Sprintf("%d", config.QuotaCPU)
	}

	if config.QuotaRAM > 0 {
		ctConfig["limits.memory"] = fmt.Sprintf("%dMB", config.QuotaRAM)
	}

	if config.QuotaProcesses > 0 {
		ctConfig["limits.processes"] = fmt.Sprintf("%d", config.QuotaProcesses)
	}

	if !config.ServerConsoleOnly {
		ctConfig["user.user-data"] = fmt.Sprintf(`#cloud-config
ssh_pwauth: True
manage_etc_hosts: True
users:
 - name: %s
	 groups: sudo
	 plain_text_passwd: %s
	 lock_passwd: False
	 shell: /bin/bash
`, containerUsername, containerPassword)
	}

	var resp *lxd.Response
	//if config.Container != "" {
	fmt.Println("Creating container form image ", containerBaseName, "with name: ", containerName)
		resp, err := lxdDaemon.LocalCopy(containerBaseName, containerName, ctConfig, nil, false)
	//} else {
	//	resp, err = lxdDaemon.Init(containerName, "local", config.Image, nil, ctConfig, nil, false)
	//}

	if err != nil {
		restStartError(w, err, containerUnknownError)
		return
	}

	err = lxdDaemon.WaitForSuccess(resp.Operation)
	if err != nil {
		restStartError(w, err, containerUnknownError)
		return
	}

	// Configure the container devices
	ct, err := lxdDaemon.ContainerInfo(containerName)
	if err != nil {
		lxdForceDelete(lxdDaemon, containerName)
		restStartError(w, err, containerUnknownError)
		return
	}
	if config.QuotaDisk > 0 {
		ct.Devices["root"] = shared.Device{"type": "disk", "path": "/", "size": fmt.Sprintf("%dGB", config.QuotaDisk)}
	}

	err = lxdDaemon.UpdateContainerConfig(containerName, ct.Brief())
	if err != nil {
		lxdForceDelete(lxdDaemon, containerName)
		restStartError(w, err, containerUnknownError)
		return
	}

	// Start the container
	resp, err = lxdDaemon.Action(containerName, "start", -1, false, false)
	if err != nil {
		lxdForceDelete(lxdDaemon, containerName)
		restStartError(w, err, containerUnknownError)
		return
	}

	err = lxdDaemon.WaitForSuccess(resp.Operation)
	if err != nil {
		lxdForceDelete(lxdDaemon, containerName)
		restStartError(w, err, containerUnknownError)
		return
	}

	// Get the IP (30s timeout)
	var containerIP string
	if !config.ServerConsoleOnly {
		time.Sleep(2 * time.Second)
		timeout := 30
		for timeout != 0 {
			timeout--
			ct, err := lxdDaemon.ContainerState(containerName)
			if err != nil {
				lxdForceDelete(lxdDaemon, containerName)
				restStartError(w, err, containerUnknownError)
				return
			}

			for netName, net := range ct.Network {
				if !shared.StringInSlice(netName, []string{"eth0", "lxcbr0"}) {
					continue
				}

				for _, addr := range net.Addresses {
					if addr.Address == "" {
						continue
					}

					if addr.Scope != "global" {
						continue
					}

					if config.ServerIPv6Only && addr.Family != "inet6" {
						continue
					}

					containerIP = addr.Address
					break
				}

				if containerIP != "" {
					break
				}
			}

			if containerIP != "" {
				break
			}

			time.Sleep(500 * time.Millisecond)
		}
	} else {
		containerIP = "console-only"
	}

	containerExpiry := time.Now().Unix() + int64(config.QuotaTime)

	if !config.ServerConsoleOnly {
		body["ip"] = containerIP
		body["username"] = containerUsername
		body["password"] = containerPassword
		body["fqdn"] = fmt.Sprintf("%s.lxd", containerName)
	}
	body["id"] = id
	body["expiry"] = containerExpiry

	// Setup cleanup code
	duration, err := time.ParseDuration(fmt.Sprintf("%ds", config.QuotaTime))
	if err != nil {
		lxdForceDelete(lxdDaemon, containerName)
		restStartError(w, err, containerUnknownError)
		return
	}

	requestTerms := ""
	containerID, err := dbNew(id, userId, containerBaseName, containerName, containerIP, containerUsername, containerPassword, containerExpiry, requestDate, requestIP, requestTerms)
	if err != nil {
		lxdForceDelete(lxdDaemon, containerName)
		restStartError(w, err, containerUnknownError)
		return
	}

	time.AfterFunc(duration, func() {
		lxdForceDelete(lxdDaemon, containerName)
		dbExpire(containerID)
	})

	// Return to the client
	body["status"] = containerStarted
	err = json.NewEncoder(w).Encode(body)
	if err != nil {
		lxdForceDelete(lxdDaemon, containerName)
		http.Error(w, "Internal server error", 500)
		return
	}
}



func initialContainerCleanupHandler() error {
	// Restore cleanup handler for existing containers
	containers, err := dbActiveContainer()
	if err != nil {
		return err
	}

	for _, entry := range containers {
		containerID := int64(entry[0].(int))
		containerName := entry[1].(string)
		containerExpiry := int64(entry[2].(int))

		duration := containerExpiry - time.Now().Unix()
		timeDuration, err := time.ParseDuration(fmt.Sprintf("%ds", duration))
		if err != nil || duration <= 0 {
			lxdForceDelete(lxdDaemon, containerName)
			dbExpire(containerID)
			continue
		}

		time.AfterFunc(timeDuration, func() {
			lxdForceDelete(lxdDaemon, containerName)
			dbExpire(containerID)
		})
	}

	return nil
}