/*
 * SPDX-License-Identifier: Apache-2.0
 *
 * The HAMi Contributors require contributions made to
 * this file be licensed under the Apache-2.0 license or a
 * compatible open source license.
 */

/*
 * Licensed to NVIDIA CORPORATION under one or more contributor
 * license agreements. See the NOTICE file distributed with
 * this work for additional information regarding copyright
 * ownership. NVIDIA CORPORATION licenses this file to you under
 * the Apache License, Version 2.0 (the "License"); you may
 * not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing,
 * software distributed under the License is distributed on an
 * "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
 * KIND, either express or implied.  See the License for the
 * specific language governing permissions and limitations
 * under the License.
 */

/*
 * Modifications Copyright The HAMi Authors. See
 * GitHub history for details.
 */

package plugin

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	spec "github.com/NVIDIA/k8s-device-plugin/api/config/v1"
	"github.com/google/uuid"
	"github.com/imdario/mergo"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/klog/v2"
	kubeletdevicepluginv1beta1 "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
	cdiapi "tags.cncf.io/container-device-interface/pkg/cdi"

	"github.com/Project-HAMi/HAMi/pkg/device"
	"github.com/Project-HAMi/HAMi/pkg/device-plugin/nvidiadevice/nvinternal/cdi"
	"github.com/Project-HAMi/HAMi/pkg/device-plugin/nvidiadevice/nvinternal/rm"
	"github.com/Project-HAMi/HAMi/pkg/device/nvidia"
	"github.com/Project-HAMi/HAMi/pkg/util"
)

// Constants for use by the 'volume-mounts' device list strategy
const (
	deviceListAsVolumeMountsHostPath          = "/dev/null"
	deviceListAsVolumeMountsContainerPathRoot = "/var/run/nvidia-container-devices"
	NodeLockNvidia                            = "hami.io/mutex.lock"
	ConfigFilePath                            = "/config/config.json"
)

var (
	hostHookPath string
	ConfigFile   *string
)

func init() {
	hostHookPath, _ = os.LookupEnv("HOOK_PATH")
}

// NvidiaDevicePlugin implements the Kubernetes device plugin API
type NvidiaDevicePlugin struct {
	rm                   rm.ResourceManager
	config               *nvidia.DeviceConfig
	deviceListEnvvar     string
	deviceListStrategies spec.DeviceListStrategies
	socket               string
	schedulerConfig      nvidia.NvidiaConfig

	applyMutex                 sync.Mutex
	disableHealthChecks        chan bool
	ackDisableHealthChecks     chan bool
	disableWatchAndRegister    chan bool
	ackDisableWatchAndRegister chan bool

	cdiHandler          cdi.Interface
	cdiEnabled          bool
	cdiAnnotationPrefix string

	operatingMode string
	migCurrent    nvidia.MigPartedSpec

	server *grpc.Server
	health chan *rm.Device
	stop   chan any
}

func readFromConfigFile(sConfig *nvidia.NvidiaConfig, path string) (string, error) {
	jsonbyte, err := os.ReadFile(path)
	mode := "hami-core"
	if err != nil {
		return "", err
	}
	var deviceConfigs nvidia.DevicePluginConfigs
	err = json.Unmarshal(jsonbyte, &deviceConfigs)
	if err != nil {
		return "", err
	}
	klog.Infof("Device Plugin Configs: %v", fmt.Sprintf("%v", deviceConfigs))
	for _, val := range deviceConfigs.Nodeconfig {
		if os.Getenv(util.NodeNameEnvName) == val.Name {
			klog.Infof("Reading config from file %s", val.Name)
			if err := mergo.Merge(&sConfig.NodeDefaultConfig, val.NodeDefaultConfig, mergo.WithOverride); err != nil {
				return "", err
			}
			if val.FilterDevice != nil && (len(val.FilterDevice.UUID) > 0 || len(val.FilterDevice.Index) > 0) {
				nvidia.DevicePluginFilterDevice = val.FilterDevice
			}
			if len(val.OperatingMode) > 0 {
				mode = val.OperatingMode
			}
			klog.Infof("FilterDevice: %v", val.FilterDevice)
		}
	}
	return mode, nil
}

func LoadNvidiaDevicePluginConfig() (*device.Config, string, error) {
	sConfig, err := device.LoadConfig(*ConfigFile)
	if err != nil {
		klog.Fatalf(`failed to load device config file %s: %v`, *ConfigFile, err)
	}
	mode, err := readFromConfigFile(&sConfig.NvidiaConfig, ConfigFilePath)
	if err != nil {
		klog.Errorf("readFromConfigFile err:%s", err.Error())
	}
	return sConfig, mode, nil
}

// NewNvidiaDevicePlugin returns an initialized NvidiaDevicePlugin
func NewNvidiaDevicePlugin(config *nvidia.DeviceConfig, resourceManager rm.ResourceManager, cdiHandler cdi.Interface, cdiEnabled bool, sConfig *device.Config, mode string) *NvidiaDevicePlugin {
	_, name := resourceManager.Resource().Split()

	deviceListStrategies, _ := spec.NewDeviceListStrategies(*config.Flags.Plugin.DeviceListStrategy)

	klog.Infoln("reading config=", config, "resourceName", config.ResourceName, "configfile=", *ConfigFile, "sconfig=", sConfig)

	// Initialize devices with configuration
	if err := device.InitDevicesWithConfig(sConfig); err != nil {
		klog.Fatalf("failed to initialize devices: %v", err)
	}
	return &NvidiaDevicePlugin{
		rm:                         resourceManager,
		config:                     config,
		deviceListEnvvar:           "NVIDIA_VISIBLE_DEVICES",
		deviceListStrategies:       deviceListStrategies,
		applyMutex:                 sync.Mutex{},
		disableHealthChecks:        nil,
		ackDisableHealthChecks:     nil,
		disableWatchAndRegister:    nil,
		ackDisableWatchAndRegister: nil,
		socket:                     kubeletdevicepluginv1beta1.DevicePluginPath + "nvidia-" + name + ".sock",
		cdiHandler:                 cdiHandler,
		cdiEnabled:                 cdiEnabled,
		cdiAnnotationPrefix:        *config.Flags.Plugin.CDIAnnotationPrefix,
		schedulerConfig:            sConfig.NvidiaConfig,
		operatingMode:              mode,
		migCurrent:                 nvidia.MigPartedSpec{},

		// These will be reinitialized every
		// time the plugin server is restarted.
		server: nil,
		health: nil,
		stop:   nil,
	}
}

func (plugin *NvidiaDevicePlugin) initialize() {
	plugin.server = grpc.NewServer([]grpc.ServerOption{}...)
	plugin.health = make(chan *rm.Device)
	plugin.stop = make(chan any)
	plugin.disableHealthChecks = make(chan bool, 1)
	plugin.ackDisableHealthChecks = make(chan bool, 1)
	plugin.disableWatchAndRegister = make(chan bool, 1)
	plugin.ackDisableWatchAndRegister = make(chan bool, 1)
}

func (plugin *NvidiaDevicePlugin) cleanup() {
	close(plugin.stop)
	plugin.server = nil
	plugin.health = nil
	plugin.stop = nil
	plugin.disableHealthChecks = nil
	plugin.ackDisableHealthChecks = nil
	plugin.disableWatchAndRegister = nil
	plugin.ackDisableWatchAndRegister = nil
}

// Devices returns the full set of devices associated with the plugin.
func (plugin *NvidiaDevicePlugin) Devices() rm.Devices {
	return plugin.rm.Devices()
}

// Start starts the gRPC server, registers the device plugin with the Kubelet,
// and starts the device healthchecks.
func (plugin *NvidiaDevicePlugin) Start() error {
	plugin.initialize()

	deviceNumbers, err := GetDeviceNums()
	if err != nil {
		return err
	}

	deviceNames, err := GetDeviceNames()
	if err != nil {
		return err
	}

	err = plugin.Serve()
	if err != nil {
		klog.Infof("Could not start device plugin for '%s': %s", plugin.rm.Resource(), err)
		plugin.cleanup()
		return err
	}
	klog.Infof("Starting to serve '%s' on %s", plugin.rm.Resource(), plugin.socket)

	err = plugin.Register()
	if err != nil {
		klog.Infof("Could not register device plugin: %s", err)
		plugin.Stop()
		return err
	}
	klog.Infof("Registered device plugin for '%s' with Kubelet", plugin.rm.Resource())
	// Prepare the lock file sub directory.Due to the sequence of startup processes, both the device plugin
	// and the vGPU monitor should attempt to create this directory by default to ensure its creation.
	err = CreateMigApplyLockDir()
	if err != nil {
		klog.Fatalf("CreateMIGLockSubDir failed:%v", err)
	}

	// If the temporary lock file still exists, it may be a leftover from the last incomplete mig  application process.
	// Delete the temporary lock file to make sure vgpu monitor can start.
	err = RemoveMigApplyLock()
	if err != nil {
		klog.Fatalf("RemoveMigApplyLock failed:%v", err)
	}

	var deviceSupportMig bool
	for _, name := range deviceNames {
		deviceSupportMig = false
		for _, migTemplate := range plugin.schedulerConfig.MigGeometriesList {
			if containsModel(name, migTemplate.Models) {
				deviceSupportMig = true
				break
			}
		}
		if !deviceSupportMig {
			break
		}
	}
	if deviceSupportMig {
		cmd := exec.Command("nvidia-mig-parted", "export")
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		err := cmd.Run()
		if err != nil {
			klog.Fatalf("nvidia-mig-parted failed with %s\n", err)
		}
		outStr := stdout.Bytes()
		yaml.Unmarshal(outStr, &plugin.migCurrent)
		os.WriteFile("/tmp/migconfig.yaml", outStr, os.ModePerm)
		if plugin.operatingMode == "mig" {
			HamiInitMigConfig, err := plugin.processMigConfigs(plugin.migCurrent.MigConfigs, deviceNumbers)
			if err != nil {
				klog.Infof("no device in node:%v", err)
			}
			plugin.migCurrent.MigConfigs["current"] = HamiInitMigConfig
			klog.Infoln("Open Mig export", plugin.migCurrent)
		} else {
			plugin.migCurrent.MigConfigs = make(map[string]nvidia.MigConfigSpecSlice)
			configSlice := nvidia.MigConfigSpecSlice{}
			for i := 0; i < deviceNumbers; i++ {
				conf := nvidia.MigConfigSpec{MigEnabled: false, Devices: []int32{int32(i)}}
				configSlice = append(configSlice, conf)
			}
			plugin.migCurrent.MigConfigs["current"] = configSlice
			klog.Infoln("Close Mig export", plugin.migCurrent)
		}
	}
	go func() {
		err := plugin.rm.CheckHealth(plugin.stop, plugin.health, plugin.disableHealthChecks, plugin.ackDisableHealthChecks)
		if err != nil {
			klog.Infof("Failed to start health check: %v; continuing with health checks disabled", err)
		}
	}()

	go func() {
		plugin.WatchAndRegister(plugin.disableWatchAndRegister, plugin.ackDisableWatchAndRegister)
	}()

	if deviceSupportMig {
		plugin.ApplyMigTemplate()
	}

	return nil
}

// Stop stops the gRPC server.
func (plugin *NvidiaDevicePlugin) Stop() error {
	if plugin == nil || plugin.server == nil {
		return nil
	}
	klog.Infof("Stopping to serve '%s' on %s", plugin.rm.Resource(), plugin.socket)
	plugin.server.Stop()
	if err := os.Remove(plugin.socket); err != nil && !os.IsNotExist(err) {
		return err
	}
	plugin.cleanup()
	return nil
}

// Serve starts the gRPC server of the device plugin.
func (plugin *NvidiaDevicePlugin) Serve() error {
	os.Remove(plugin.socket)
	sock, err := net.Listen("unix", plugin.socket)
	if err != nil {
		return err
	}

	kubeletdevicepluginv1beta1.RegisterDevicePluginServer(plugin.server, plugin)

	go func() {
		lastCrashTime := time.Now()
		restartCount := 0
		for {
			klog.Infof("Starting GRPC server for '%s'", plugin.rm.Resource())
			err := plugin.server.Serve(sock)
			if err == nil {
				break
			}

			klog.Infof("GRPC server for '%s' crashed with error: %v", plugin.rm.Resource(), err)

			// restart if it has not been too often
			// i.e. if server has crashed more than 5 times and it didn't last more than one hour each time
			if restartCount > 5 {
				// quit
				klog.Fatalf("GRPC server for '%s' has repeatedly crashed recently. Quitting", plugin.rm.Resource())
			}
			timeSinceLastCrash := time.Since(lastCrashTime).Seconds()
			lastCrashTime = time.Now()
			if timeSinceLastCrash > 3600 {
				// it has been one hour since the last crash.. reset the count
				// to reflect on the frequency
				restartCount = 1
			} else {
				restartCount++
			}
		}
	}()

	// Wait for server to start by launching a blocking connexion
	conn, err := plugin.dial(plugin.socket, 5*time.Second)
	if err != nil {
		return err
	}
	conn.Close()

	return nil
}

// Register registers the device plugin for the given resourceName with Kubelet.
func (plugin *NvidiaDevicePlugin) Register() error {
	conn, err := plugin.dial(kubeletdevicepluginv1beta1.KubeletSocket, 5*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()

	client := kubeletdevicepluginv1beta1.NewRegistrationClient(conn)
	reqt := &kubeletdevicepluginv1beta1.RegisterRequest{
		Version:      kubeletdevicepluginv1beta1.Version,
		Endpoint:     path.Base(plugin.socket),
		ResourceName: string(plugin.rm.Resource()),
		Options: &kubeletdevicepluginv1beta1.DevicePluginOptions{
			GetPreferredAllocationAvailable: false,
		},
	}

	_, err = client.Register(context.Background(), reqt)
	if err != nil {
		return err
	}
	return nil
}

// GetDevicePluginOptions returns the values of the optional settings for this plugin
func (plugin *NvidiaDevicePlugin) GetDevicePluginOptions(context.Context, *kubeletdevicepluginv1beta1.Empty) (*kubeletdevicepluginv1beta1.DevicePluginOptions, error) {
	options := &kubeletdevicepluginv1beta1.DevicePluginOptions{
		GetPreferredAllocationAvailable: false,
	}
	return options, nil
}

// ListAndWatch lists devices and update that list according to the health status
func (plugin *NvidiaDevicePlugin) ListAndWatch(e *kubeletdevicepluginv1beta1.Empty, s kubeletdevicepluginv1beta1.DevicePlugin_ListAndWatchServer) error {
	s.Send(&kubeletdevicepluginv1beta1.ListAndWatchResponse{Devices: plugin.apiDevices()})

	for {
		select {
		case <-plugin.stop:
			return nil
		case d := <-plugin.health:
			// FIXME: there is no way to recover from the Unhealthy state.
			d.Health = kubeletdevicepluginv1beta1.Unhealthy
			klog.Infof("'%s' device marked unhealthy: %s", plugin.rm.Resource(), d.ID)
			s.Send(&kubeletdevicepluginv1beta1.ListAndWatchResponse{Devices: plugin.apiDevices()})
		}
	}
}

// GetPreferredAllocation returns the preferred allocation from the set of devices specified in the request
func (plugin *NvidiaDevicePlugin) GetPreferredAllocation(ctx context.Context, r *kubeletdevicepluginv1beta1.PreferredAllocationRequest) (*kubeletdevicepluginv1beta1.PreferredAllocationResponse, error) {
	response := &kubeletdevicepluginv1beta1.PreferredAllocationResponse{}
	/*for _, req := range r.ContainerRequests {
		devices, err := plugin.rm.GetPreferredAllocation(req.AvailableDeviceIDs, req.MustIncludeDeviceIDs, int(req.AllocationSize))
		if err != nil {
			return nil, fmt.Errorf("error getting list of preferred allocation devices: %v", err)
		}

		resp := &kubeletdevicepluginv1beta1.ContainerPreferredAllocationResponse{
			DeviceIDs: devices,
		}

		response.ContainerResponses = append(response.ContainerResponses, resp)
	}*/
	return response, nil
}

// Allocate which return list of devices.
func (plugin *NvidiaDevicePlugin) Allocate(ctx context.Context, reqs *kubeletdevicepluginv1beta1.AllocateRequest) (*kubeletdevicepluginv1beta1.AllocateResponse, error) {
	klog.InfoS("Allocate", "request", reqs)
	responses := kubeletdevicepluginv1beta1.AllocateResponse{}
	nodename := os.Getenv(util.NodeNameEnvName)
	current, err := util.GetPendingPod(ctx, nodename)
	if err != nil {
		//nodelock.ReleaseNodeLock(nodename, NodeLockNvidia, current)
		return &kubeletdevicepluginv1beta1.AllocateResponse{}, err
	}
	klog.Infof("Allocate pod name is %s/%s, annotation is %+v", current.Namespace, current.Name, current.Annotations)

	for idx, req := range reqs.ContainerRequests {
		// If the devices being allocated are replicas, then (conditionally)
		// error out if more than one resource is being allocated.

		if strings.Contains(req.DevicesIDs[0], "MIG") {
			if plugin.config.Sharing.TimeSlicing.FailRequestsGreaterThanOne && rm.AnnotatedIDs(req.DevicesIDs).AnyHasAnnotations() {
				if len(req.DevicesIDs) > 1 {
					device.PodAllocationFailed(nodename, current, NodeLockNvidia)
					return nil, fmt.Errorf("request for '%v: %v' too large: maximum request size for shared resources is 1", plugin.rm.Resource(), len(req.DevicesIDs))
				}
			}

			for _, id := range req.DevicesIDs {
				if !plugin.rm.Devices().Contains(id) {
					device.PodAllocationFailed(nodename, current, NodeLockNvidia)
					return nil, fmt.Errorf("invalid allocation request for '%s': unknown device: %s", plugin.rm.Resource(), id)
				}
			}

			response, err := plugin.getAllocateResponse(req.DevicesIDs)
			if err != nil {
				device.PodAllocationFailed(nodename, current, NodeLockNvidia)
				return nil, fmt.Errorf("failed to get allocate response: %v", err)
			}
			responses.ContainerResponses = append(responses.ContainerResponses, response)
		} else {
			currentCtr, devreq, err := GetNextDeviceRequest(nvidia.NvidiaGPUDevice, *current)
			klog.Infoln("deviceAllocateFromAnnotation=", devreq)
			if err != nil {
				device.PodAllocationFailed(nodename, current, NodeLockNvidia)
				return &kubeletdevicepluginv1beta1.AllocateResponse{}, err
			}
			if len(devreq) != len(reqs.ContainerRequests[idx].DevicesIDs) {
				device.PodAllocationFailed(nodename, current, NodeLockNvidia)
				return &kubeletdevicepluginv1beta1.AllocateResponse{}, errors.New("device number not matched")
			}
			response, err := plugin.getAllocateResponse(plugin.GetContainerDeviceStrArray(devreq))
			if err != nil {
				return nil, fmt.Errorf("failed to get allocate response: %v", err)
			}

			err = EraseNextDeviceTypeFromAnnotation(nvidia.NvidiaGPUDevice, *current)
			if err != nil {
				device.PodAllocationFailed(nodename, current, NodeLockNvidia)
				return &kubeletdevicepluginv1beta1.AllocateResponse{}, err
			}

			if plugin.operatingMode != "mig" {
				for i, dev := range devreq {
					limitKey := fmt.Sprintf("CUDA_DEVICE_MEMORY_LIMIT_%v", i)
					response.Envs[limitKey] = fmt.Sprintf("%vm", dev.Usedmem)
				}
				response.Envs["CUDA_DEVICE_SM_LIMIT"] = fmt.Sprint(devreq[0].Usedcores)
				response.Envs["CUDA_DEVICE_MEMORY_SHARED_CACHE"] = fmt.Sprintf("%s/vgpu/%v.cache", hostHookPath, uuid.New().String())
				if *plugin.schedulerConfig.DeviceMemoryScaling > 1 {
					response.Envs["CUDA_OVERSUBSCRIBE"] = "true"
				}
				if *plugin.schedulerConfig.LogLevel != "" {
					response.Envs["LIBCUDA_LOG_LEVEL"] = string(*plugin.schedulerConfig.LogLevel)
				}
				if plugin.schedulerConfig.DisableCoreLimit {
					response.Envs[util.CoreLimitSwitch] = "disable"
				}
				cacheFileHostDirectory := fmt.Sprintf("%s/vgpu/containers/%s_%s", hostHookPath, current.UID, currentCtr.Name)
				os.RemoveAll(cacheFileHostDirectory)

				os.MkdirAll(cacheFileHostDirectory, 0777)
				os.Chmod(cacheFileHostDirectory, 0777)
				os.MkdirAll("/tmp/vgpulock", 0777)
				os.Chmod("/tmp/vgpulock", 0777)
				response.Mounts = append(response.Mounts,
					&kubeletdevicepluginv1beta1.Mount{ContainerPath: fmt.Sprintf("%s/vgpu/libvgpu.so", hostHookPath),
						HostPath: GetLibPath(),
						ReadOnly: true},
					&kubeletdevicepluginv1beta1.Mount{ContainerPath: fmt.Sprintf("%s/vgpu", hostHookPath),
						HostPath: cacheFileHostDirectory,
						ReadOnly: false},
					&kubeletdevicepluginv1beta1.Mount{ContainerPath: "/tmp/vgpulock",
						HostPath: "/tmp/vgpulock",
						ReadOnly: false},
				)
				found := false
				for _, val := range currentCtr.Env {
					if strings.Compare(val.Name, "CUDA_DISABLE_CONTROL") == 0 {
						// if env existed but is set to false or can not be parsed, ignore
						t, _ := strconv.ParseBool(val.Value)
						if !t {
							continue
						}
						// only env existed and set to true, we mark it "found"
						found = true
						break
					}
				}
				if !found {
					response.Mounts = append(response.Mounts, &kubeletdevicepluginv1beta1.Mount{ContainerPath: "/etc/ld.so.preload",
						HostPath: hostHookPath + "/vgpu/ld.so.preload",
						ReadOnly: true},
					)
				}
				_, err = os.Stat(fmt.Sprintf("%s/vgpu/license", hostHookPath))
				if err == nil {
					response.Mounts = append(response.Mounts, &kubeletdevicepluginv1beta1.Mount{
						ContainerPath: "/tmp/license",
						HostPath:      fmt.Sprintf("%s/vgpu/license", hostHookPath),
						ReadOnly:      true,
					})
					response.Mounts = append(response.Mounts, &kubeletdevicepluginv1beta1.Mount{
						ContainerPath: "/usr/bin/vgpuvalidator",
						HostPath:      fmt.Sprintf("%s/vgpu/vgpuvalidator", hostHookPath),
						ReadOnly:      true,
					})
				}
			}
			responses.ContainerResponses = append(responses.ContainerResponses, response)
		}
	}
	klog.Infoln("Allocate Response", responses.ContainerResponses)
	device.PodAllocationTrySuccess(nodename, nvidia.NvidiaGPUDevice, NodeLockNvidia, current)
	return &responses, nil
}

func (plugin *NvidiaDevicePlugin) getAllocateResponse(requestIds []string) (*kubeletdevicepluginv1beta1.ContainerAllocateResponse, error) {
	deviceIDs := plugin.deviceIDsFromAnnotatedDeviceIDs(requestIds)

	responseID := uuid.New().String()
	response, err := plugin.getAllocateResponseForCDI(responseID, deviceIDs)
	if err != nil {
		return nil, fmt.Errorf("failed to get allocate response for CDI: %v", err)
	}

	response.Envs = plugin.apiEnvs(plugin.deviceListEnvvar, deviceIDs)
	//if plugin.deviceListStrategies.Includes(spec.DeviceListStrategyVolumeMounts) || plugin.deviceListStrategies.Includes(spec.DeviceListStrategyEnvvar) {
	//	response.Envs = plugin.apiEnvs(plugin.deviceListEnvvar, deviceIDs)
	//}
	/*
		if plugin.deviceListStrategies.Includes(spec.DeviceListStrategyVolumeMounts) {
			response.Envs = plugin.apiEnvs(plugin.deviceListEnvvar, []string{deviceListAsVolumeMountsContainerPathRoot})
			response.Mounts = plugin.apiMounts(deviceIDs)
		}*/
	if *plugin.config.Flags.Plugin.PassDeviceSpecs {
		response.Devices = plugin.apiDeviceSpecs(*plugin.config.Flags.NvidiaDriverRoot, requestIds)
	}
	if *plugin.config.Flags.GDSEnabled {
		response.Envs["NVIDIA_GDS"] = "enabled"
	}
	if *plugin.config.Flags.MOFEDEnabled {
		response.Envs["NVIDIA_MOFED"] = "enabled"
	}

	return &response, nil
}

// getAllocateResponseForCDI returns the allocate response for the specified device IDs.
// This response contains the annotations required to trigger CDI injection in the container engine or nvidia-container-runtime.
func (plugin *NvidiaDevicePlugin) getAllocateResponseForCDI(responseID string, deviceIDs []string) (kubeletdevicepluginv1beta1.ContainerAllocateResponse, error) {
	response := kubeletdevicepluginv1beta1.ContainerAllocateResponse{}

	if !plugin.cdiEnabled {
		return response, nil
	}

	var devices []string
	for _, id := range deviceIDs {
		devices = append(devices, plugin.cdiHandler.QualifiedName("gpu", id))
	}

	if *plugin.config.Flags.GDSEnabled {
		devices = append(devices, plugin.cdiHandler.QualifiedName("gds", "all"))
	}
	if *plugin.config.Flags.MOFEDEnabled {
		devices = append(devices, plugin.cdiHandler.QualifiedName("mofed", "all"))
	}

	if len(devices) == 0 {
		return response, nil
	}

	if plugin.deviceListStrategies.Includes(spec.DeviceListStrategyCDIAnnotations) {
		annotations, err := plugin.getCDIDeviceAnnotations(responseID, devices)
		if err != nil {
			return response, err
		}
		response.Annotations = annotations
	}

	return response, nil
}

func (plugin *NvidiaDevicePlugin) getCDIDeviceAnnotations(id string, devices []string) (map[string]string, error) {
	annotations, err := cdiapi.UpdateAnnotations(map[string]string{}, "nvidia-device-plugin", id, devices)
	if err != nil {
		return nil, fmt.Errorf("failed to add CDI annotations: %v", err)
	}

	if plugin.cdiAnnotationPrefix == spec.DefaultCDIAnnotationPrefix {
		return annotations, nil
	}

	// update annotations if a custom CDI prefix is configured
	updatedAnnotations := make(map[string]string)
	for k, v := range annotations {
		newKey := plugin.cdiAnnotationPrefix + strings.TrimPrefix(k, spec.DefaultCDIAnnotationPrefix)
		updatedAnnotations[newKey] = v
	}

	return updatedAnnotations, nil
}

// PreStartContainer is unimplemented for this plugin
func (plugin *NvidiaDevicePlugin) PreStartContainer(context.Context, *kubeletdevicepluginv1beta1.PreStartContainerRequest) (*kubeletdevicepluginv1beta1.PreStartContainerResponse, error) {
	return &kubeletdevicepluginv1beta1.PreStartContainerResponse{}, nil
}

// dial establishes the gRPC communication with the registered device plugin.
func (plugin *NvidiaDevicePlugin) dial(unixSocketPath string, timeout time.Duration) (*grpc.ClientConn, error) {
	c, err := grpc.Dial(unixSocketPath, grpc.WithInsecure(), grpc.WithBlock(),
		grpc.WithTimeout(timeout),
		grpc.WithDialer(func(addr string, timeout time.Duration) (net.Conn, error) {
			return net.DialTimeout("unix", addr, timeout)
		}),
	)

	if err != nil {
		return nil, err
	}

	return c, nil
}

func (plugin *NvidiaDevicePlugin) deviceIDsFromAnnotatedDeviceIDs(ids []string) []string {
	var deviceIDs []string
	if *plugin.config.Flags.Plugin.DeviceIDStrategy == spec.DeviceIDStrategyUUID {
		deviceIDs = rm.AnnotatedIDs(ids).GetIDs()
	}
	if *plugin.config.Flags.Plugin.DeviceIDStrategy == spec.DeviceIDStrategyIndex {
		deviceIDs = plugin.rm.Devices().Subset(ids).GetIndices()
	}
	return deviceIDs
}

func (plugin *NvidiaDevicePlugin) apiDevices() []*kubeletdevicepluginv1beta1.Device {
	return plugin.rm.Devices().GetPluginDevices(*plugin.schedulerConfig.DeviceSplitCount)
}

func (plugin *NvidiaDevicePlugin) apiEnvs(envvar string, deviceIDs []string) map[string]string {
	return map[string]string{
		envvar: strings.Join(deviceIDs, ","),
	}
}

func (plugin *NvidiaDevicePlugin) apiDeviceSpecs(driverRoot string, ids []string) []*kubeletdevicepluginv1beta1.DeviceSpec {
	optional := map[string]bool{
		"/dev/nvidiactl":        true,
		"/dev/nvidia-uvm":       true,
		"/dev/nvidia-uvm-tools": true,
		"/dev/nvidia-modeset":   true,
	}

	paths := plugin.rm.GetDevicePaths(ids)

	var specs []*kubeletdevicepluginv1beta1.DeviceSpec
	for _, p := range paths {
		if optional[p] {
			if _, err := os.Stat(p); err != nil {
				continue
			}
		}
		spec := &kubeletdevicepluginv1beta1.DeviceSpec{
			ContainerPath: p,
			HostPath:      filepath.Join(driverRoot, p),
			Permissions:   "rw",
		}
		specs = append(specs, spec)
	}

	return specs
}

func (plugin *NvidiaDevicePlugin) processMigConfigs(migConfigs map[string]nvidia.MigConfigSpecSlice, deviceCount int) (nvidia.MigConfigSpecSlice, error) {
	if migConfigs == nil {
		return nil, fmt.Errorf("migConfigs cannot be nil")
	}
	if deviceCount <= 0 {
		return nil, fmt.Errorf("deviceCount must be positive")
	}

	transformConfigs := func() (nvidia.MigConfigSpecSlice, error) {
		var result nvidia.MigConfigSpecSlice

		if len(migConfigs["current"]) == 1 && len(migConfigs["current"][0].Devices) == 0 {
			for i := 0; i < deviceCount; i++ {
				config := deepCopyMigConfig(migConfigs["current"][0])
				config.Devices = []int32{int32(i)}
				result = append(result, config)
			}
			return result, nil
		}

		deviceToConfig := make(map[int32]*nvidia.MigConfigSpec)
		for i := range migConfigs["current"] {
			for _, device := range migConfigs["current"][i].Devices {
				deviceToConfig[device] = &migConfigs["current"][i]
			}
		}

		for i := 0; i < deviceCount; i++ {
			deviceIndex := int32(i)
			config, exists := deviceToConfig[deviceIndex]
			if !exists {
				return nil, fmt.Errorf("device %d does not match any MIG configuration", i)
			}
			newConfig := deepCopyMigConfig(*config)
			newConfig.Devices = []int32{deviceIndex}
			result = append(result, newConfig)

		}
		return result, nil
	}

	return transformConfigs()
}
