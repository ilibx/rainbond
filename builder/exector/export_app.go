// RAINBOND, Application Management Platform
// Copyright (C) 2014-2017 Goodrain Co., Ltd.

// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version. For any non-GPL usage of Rainbond,
// one or multiple Commercial Licenses authorized by Goodrain Co., Ltd.
// must be obtained first.

// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU General Public License for more details.

// You should have received a copy of the GNU General Public License
// along with this program. If not, see <http://www.gnu.org/licenses/>.

package exector

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/docker/docker/client"
	ramv1alpha1 "github.com/goodrain/rainbond-oam/pkg/ram/v1alpha1"
	"github.com/goodrain/rainbond/builder"
	"github.com/goodrain/rainbond/builder/sources"
	"github.com/goodrain/rainbond/db"
	"github.com/goodrain/rainbond/event"
	"github.com/goodrain/rainbond/util"
	"github.com/goodrain/rainbond/util/envutil"
	pinyin "github.com/mozillazg/go-pinyin"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	yaml "gopkg.in/yaml.v2"
)

var re = regexp.MustCompile(`\s`)

//ExportApp Export app to specified format(rainbond-app or dockercompose)
type ExportApp struct {
	EventID      string `json:"event_id"`
	Format       string `json:"format"`
	SourceDir    string `json:"source_dir"`
	Logger       event.Logger
	DockerClient *client.Client
}

func init() {
	RegisterWorker("export_app", NewExportApp)
}

//NewExportApp create
func NewExportApp(in []byte, m *exectorManager) (TaskWorker, error) {
	eventID := gjson.GetBytes(in, "event_id").String()
	logger := event.GetManager().GetLogger(eventID)
	return &ExportApp{
		Format:       gjson.GetBytes(in, "format").String(),
		SourceDir:    gjson.GetBytes(in, "source_dir").String(),
		Logger:       logger,
		EventID:      eventID,
		DockerClient: m.DockerClient,
	}, nil
}

//Run Run
func (i *ExportApp) Run(timeout time.Duration) error {
	if i.Format == "rainbond-app" {
		err := i.exportRainbondAPP()
		if err != nil {
			i.updateStatus("failed")
		}
		return err
	} else if i.Format == "docker-compose" {
		err := i.exportDockerCompose()
		if err != nil {
			i.updateStatus("failed")
		}
		return err
	}
	return errors.New("Unsupported the format: " + i.Format)
}

// exportRainbondAPP export offline rainbond app
func (i *ExportApp) exportRainbondAPP() error {
	if ok := i.isLatest(); ok {
		i.updateStatus("success")
		return nil
	}

	// Delete the old application group directory and then regenerate the application package
	if err := i.CleanSourceDir(); err != nil {
		return err
	}

	// Save application attachments
	if err := i.saveApps(); err != nil {
		return err
	}

	// Save the plugin attachments
	if err := i.savePlugins(); err != nil {
		return err
	}

	// zip all file
	if err := i.zip(); err != nil {
		return err
	}

	// update export event status
	if err := i.updateStatus("success"); err != nil {
		return err
	}

	return nil
}

//  exportDockerCompose export app to docker compose app
func (i *ExportApp) exportDockerCompose() error {
	if ok := i.isLatest(); ok {
		i.updateStatus("success")
		return nil
	}

	// Delete the old application group directory and then regenerate the application package
	if err := i.CleanSourceDir(); err != nil {
		return err
	}

	// Save components attachments
	if err := i.saveComponents(); err != nil {
		return err
	}

	// 在主目录中生成文件：docker-compose.yaml
	if err := i.buildDockerComposeYaml(); err != nil {
		return err
	}

	// 生成应用启动脚本
	if err := i.buildStartScript(); err != nil {
		return err
	}

	// 打包整个目录为tar包
	if err := i.zip(); err != nil {
		return err
	}

	// 更新应用状态
	if err := i.updateStatus("success"); err != nil {
		return err
	}

	return nil
}

//Stop stop
func (i *ExportApp) Stop() error {
	return nil
}

//Name return worker name
func (i *ExportApp) Name() string {
	return "export_app"
}

//GetLogger GetLogger
func (i *ExportApp) GetLogger() event.Logger {
	return i.Logger
}

// isLatest Returns true if the application is packaged and up to date
func (i *ExportApp) isLatest() bool {
	md5File := fmt.Sprintf("%s/metadata.json.md5", i.SourceDir)
	if _, err := os.Stat(md5File); os.IsNotExist(err) {
		logrus.Debug("The export app md5 file is not found: ", md5File)
		return false
	}
	err := exec.Command("md5sum", "-c", md5File).Run()
	if err != nil {
		tarFile := i.SourceDir + ".tar"
		if _, err := os.Stat(tarFile); os.IsNotExist(err) {
			logrus.Debug("The export app tar file is not found. ")
			return false
		}
		logrus.Info("The export app tar file is not latest.")
		return false
	}
	logrus.Info("The export app tar file is latest.")
	return true
}

//CleanSourceDir clean export dir
func (i *ExportApp) CleanSourceDir() error {
	logrus.Debug("Ready clean the source directory.")
	metaFile := fmt.Sprintf("%s/metadata.json", i.SourceDir)

	data, err := ioutil.ReadFile(metaFile)
	if err != nil {
		logrus.Error("Failed to read metadata file: ", err)
		return err
	}

	os.RemoveAll(i.SourceDir)
	os.MkdirAll(i.SourceDir, 0755)

	if err := ioutil.WriteFile(metaFile, data, 0644); err != nil {
		logrus.Error("Failed to write metadata file: ", err)
		return err
	}

	return nil
}

//parseApps get apps array from metadata.json
func (i *ExportApp) parseApps() ([]gjson.Result, error) {
	i.Logger.Info("解析应用信息", map[string]string{"step": "export-app", "status": "success"})

	data, err := ioutil.ReadFile(fmt.Sprintf("%s/metadata.json", i.SourceDir))
	if err != nil {
		i.Logger.Error("导出应用失败，没有找到应用信息", map[string]string{"step": "read-metadata", "status": "failure"})
		logrus.Error("Failed to read metadata file: ", err)
		return nil, err
	}

	arr := gjson.GetBytes(data, "apps").Array()
	if len(arr) < 1 {
		i.Logger.Error("解析应用列表信息失败", map[string]string{"step": "parse-apps", "status": "failure"})
		err := errors.New("Not found app in the metadata")
		logrus.Error("Failed to get apps from json: ", err)
		return nil, err
	}
	logrus.Debug("Successful parse apps array from metadata, count: ", len(arr))

	return arr, nil
}

// parse metadata into ram
func (i *ExportApp) parseApp() (*ramv1alpha1.RainbondApplicationConfig, error) {
	data, err := ioutil.ReadFile(fmt.Sprintf("%s/metadata.json", i.SourceDir))
	if err != nil {
		return nil, errors.Wrapf(err, "open metadata.json")
	}

	var ram ramv1alpha1.RainbondApplicationConfig
	if err := json.Unmarshal(data, &ram); err != nil {
		return nil, errors.Wrapf(err, "unmarshal data into ram")
	}

	return &ram, nil
}

func (i *ExportApp) pullImage(component *ramv1alpha1.Component) (string, error) {
	// docker pull image-name
	_, err := sources.ImagePull(i.DockerClient, component.ShareImage, component.AppImage.HubUser, component.AppImage.HubPassword, i.Logger, 15)
	if err != nil {
		return "", err
	}
	//change save app image name
	saveImageName := sources.GenSaveImageName(component.ShareImage)
	if err := sources.ImageTag(i.DockerClient, component.ShareImage, saveImageName, i.Logger, 2); err != nil {
		return "", err
	}
	return saveImageName, nil
}

//exportImage export image of app
func (i *ExportApp) exportImage(serviceDir string, app gjson.Result) error {
	os.MkdirAll(serviceDir, 0755)
	image := app.Get("share_image").String()
	tarFileName := buildToLinuxFileName(image)
	user, pass := builder.GetImageUserInfo(app.Get("service_image.hub_user").String(), app.Get("service_image.hub_password").String())
	// ignore runner image
	if checkIsRunner(image) {
		logrus.Debug("Skip the runner image: ", image)
		return nil
	}
	// docker pull image-name
	_, err := sources.ImagePull(i.DockerClient, image, user, pass, i.Logger, 15)
	if err != nil {
		return err
	}
	//change save app image name
	saveImageName := sources.GenSaveImageName(image)
	if err := sources.ImageTag(i.DockerClient, image, saveImageName, i.Logger, 2); err != nil {
		return err
	}
	// save image to tar file
	err = sources.ImageSave(i.DockerClient, saveImageName, fmt.Sprintf("%s/%s.image.tar", serviceDir, tarFileName), i.Logger)
	if err != nil {
		i.Logger.Error(fmt.Sprintf("save image to local error：%s", image),
			map[string]string{"step": "save-image", "status": "failure"})
		logrus.Error("Failed to save image: ", err)
		return err
	}
	logrus.Debug("Successful save image file: ", image)
	return nil
}

func (i *ExportApp) exportSlug(serviceDir string, app gjson.Result) error {
	shareSlugPath := app.Get("share_slug_path").String()
	serviceName := app.Get("service_cname").String()
	tarFileName := buildToLinuxFileName(shareSlugPath)
	_, err := os.Stat(shareSlugPath)
	if shareSlugPath != "" && err == nil {
		logrus.Debug("The slug file was exist already, direct copy to service dir: ", shareSlugPath)
		err = util.CopyFile(shareSlugPath, fmt.Sprintf("%s/%s", serviceDir, tarFileName))
		if err == nil {
			return nil
		}
		// if local copy failure, try download it
		logrus.Debugf("Failed to copy the slug file to service dir %s: %v", shareSlugPath, err)
	}
	// get slug save server (ftp) info
	ftpHost := app.Get("service_slug.ftp_host").String()
	ftpPort := app.Get("service_slug.ftp_port").String()
	ftpUsername := app.Get("service_slug.ftp_username").String()
	ftpPassword := app.Get("service_slug.ftp_password").String()

	ftpClient, err := sources.NewSFTPClient(ftpUsername, ftpPassword, ftpHost, ftpPort)
	if err != nil {
		logrus.Error("Failed to create ftp client: ", err)
		return err
	}
	// download slug file
	i.Logger.Info(fmt.Sprintf("Download service %s slug file", serviceName), map[string]string{"step": "get-slug", "status": "failure"})
	err = ftpClient.DownloadFile(shareSlugPath, fmt.Sprintf("%s/%s", serviceDir, tarFileName), i.Logger)
	ftpClient.Close()
	if err != nil {
		logrus.Errorf("Failed to download slug file for group %s: %v", i.SourceDir, err)
		return err
	}
	logrus.Debug("Successful download slug file: ", shareSlugPath)
	return nil
}

func (i *ExportApp) exportComponentConfigFile(serviceDir string, v ramv1alpha1.ComponentVolume) error {
	serviceDir = strings.TrimRight(serviceDir, "/")
	filename := fmt.Sprintf("%s%s", serviceDir, v.VolumeMountPath)
	dir := path.Dir(filename)
	os.MkdirAll(dir, 0755)
	return ioutil.WriteFile(filename, []byte(v.FileConent), 0644)
}

func (i *ExportApp) exportConfigFile(serviceDir string, v gjson.Result) error {
	serviceDir = strings.TrimRight(serviceDir, "/")
	fc := v.Get("file_content").String()
	vp := v.Get("volume_path").String()
	filename := fmt.Sprintf("%s%s", serviceDir, vp)
	dir := path.Dir(filename)
	os.MkdirAll(dir, 0755)
	return ioutil.WriteFile(filename, []byte(fc), 0644)
}

func (i *ExportApp) savePlugins() error {
	i.Logger.Info("Parsing plugin information", map[string]string{"step": "export-plugins", "status": "success"})

	data, err := ioutil.ReadFile(fmt.Sprintf("%s/metadata.json", i.SourceDir))
	if err != nil {
		i.Logger.Error("导出插件失败，没有找到应用信息", map[string]string{"step": "read-metadata", "status": "failure"})
		logrus.Error("Failed to read metadata file: ", err)
		return err
	}

	plugins := gjson.GetBytes(data, "plugins").Array()

	for _, plugin := range plugins {
		pluginName := plugin.Get("plugin_name").String()
		pluginName = unicode2zh(pluginName)
		pluginDir := fmt.Sprintf("%s/%s", i.SourceDir, pluginName)
		os.MkdirAll(pluginDir, 0755)
		image := plugin.Get("share_image").String()
		tarFileName := buildToLinuxFileName(image)
		user, pass := builder.GetImageUserInfo(plugin.Get("plugin_image.hub_user").String(), plugin.Get("plugin_image.hub_password").String())
		// docker pull image-name
		_, err := sources.ImagePull(i.DockerClient, image, user, pass, i.Logger, 15)
		if err != nil {
			return err
		}
		//change save app image name
		saveImageName := sources.GenSaveImageName(image)
		if err := sources.ImageTag(i.DockerClient, image, saveImageName, i.Logger, 2); err != nil {
			return err
		}
		// save image to tar file
		err = sources.ImageSave(i.DockerClient, saveImageName, fmt.Sprintf("%s/%s.image.tar", pluginDir, tarFileName), i.Logger)
		if err != nil {
			i.Logger.Error(fmt.Sprintf("save plugin image to local error：%s", image),
				map[string]string{"step": "save-plugin-image", "status": "failure"})
			logrus.Error("Failed to save plugin image: ", err)
			return err
		}
		logrus.Debug("Successful save plugin image file: ", image)
	}

	return nil
}

// save all app attachment
// dir naming rule：Convert unicode to Chinese in the component name and remove the empty，"2048\\u5e94\\u7528" -> "2048应用"
// Image naming rule: goodrain.me/percona-mysql:5.5_latest -> percona-mysqlTAG5.5_latest.image.tar
// slug naming rule: /app_publish/vzrd9po6/9d2635a7c59d4974bb4dc62f04/v1.0_20180207165207.tgz -> v1.0_20180207165207.tgz
func (i *ExportApp) saveApps() error {
	apps, err := i.parseApps()
	if err != nil {
		return err
	}

	i.Logger.Info("Start export app", map[string]string{"step": "export-app", "status": "success"})

	for _, app := range apps {
		serviceName := app.Get("service_cname").String()
		serviceName = unicode2zh(serviceName)
		serviceDir := fmt.Sprintf("%s/%s", i.SourceDir, serviceName)
		os.MkdirAll(serviceDir, 0755)
		logrus.Debug("Create directory for export app: ", serviceDir)
		shareImage := app.Get("share_image").String()

		volumes := app.Get("service_volume_map_list").Array()
		if volumes != nil && len(volumes) > 0 {
			for _, v := range volumes {
				err := i.exportConfigFile(serviceDir, v)
				if err != nil {
					logrus.Errorf("error exporting config file: %v", err)
					return err
				}
			}
		}
		if shareImage != "" {
			logrus.Infof("The service is image model deploy: %s", serviceName)
			// app is image type
			if err := i.exportImage(serviceDir, app); err != nil {
				return err
			}
			continue
		}
	}
	return nil
}

// saveComponents Bulk export of mirrored mode, lower disk footprint for the entire package
func (i *ExportApp) saveComponents() error {
	app, err := i.parseApp()
	if err != nil {
		return err
	}
	dockerCompose := newDockerCompose(app)
	i.Logger.Info(fmt.Sprintf("Start export app %s", app.AppName), map[string]string{"step": "export-app", "status": "success"})
	var componentImageNames []string
	for _, component := range app.Components {
		componentName := component.ServiceCname
		componentEnName := dockerCompose.GetServiceName(component.ServiceShareID)
		serviceDir := fmt.Sprintf("%s/%s", i.SourceDir, componentEnName)
		os.MkdirAll(serviceDir, 0755)
		logrus.Debugf("Create directory for export app: %s", serviceDir)
		volumes := component.ServiceVolumeMapList
		if volumes != nil && len(volumes) > 0 {
			for _, v := range volumes {
				if v.VolumeType == ramv1alpha1.ConfigFileVolumeType {
					err := i.exportComponentConfigFile(serviceDir, v)
					if err != nil {
						logrus.Errorf("error exporting config file: %v", err)
						return err
					}
				}
			}
		}
		if component.ShareImage != "" {
			// app is image type
			localImageName, err := i.pullImage(component)
			if err != nil {
				return err
			}
			logrus.Infof("Pull component %s image success", componentName)
			componentImageNames = append(componentImageNames, localImageName)
		}
	}
	return i.saveComponentImages(componentImageNames)
}

// unicode2zh 将unicode转为中文，并去掉空格
func unicode2zh(uText string) (context string) {
	for i, char := range strings.Split(uText, `\\u`) {
		if i < 1 {
			context = char
			continue
		}

		length := len(char)
		if length > 3 {
			pre := char[:4]
			zh, err := strconv.ParseInt(pre, 16, 32)
			if err != nil {
				context += char
				continue
			}

			context += fmt.Sprintf("%c", zh)

			if length > 4 {
				context += char[4:]
			}
		}

	}

	context = strings.TrimSpace(context)

	return context
}

// [a-zA-Z0-9._-]
func composeName(uText string) string {
	str := unicode2zh(uText)

	var res string
	for _, runeValue := range str {
		if unicode.Is(unicode.Han, runeValue) {
			// convert chinese to pinyin
			res += strings.Join(pinyin.LazyConvert(string(runeValue), nil), "")
			continue
		}
		matched, err := regexp.Match("[a-zA-Z0-9._-]", []byte{byte(runeValue)})
		if err != nil {
			logrus.Warningf("check if %s meets [a-zA-Z0-9._-]: %v", string(runeValue), err)
		}
		if !matched {
			res += "_"
			continue
		}
		res += string(runeValue)
	}
	logrus.Debugf("convert chinese %s to pinyin %s", str, res)
	return res
}

func checkIsRunner(image string) bool {
	return strings.Contains(image, builder.RUNNERIMAGENAME)
}

func (i *ExportApp) exportRunnerImage() error {
	isExist := false
	var image, tarFileName string
	logrus.Debug("Ready export runner image")
	apps, err := i.parseApps()
	if err != nil {
		return err
	}
	for _, app := range apps {
		image = app.Get("image").String()
		tarFileName = buildToLinuxFileName(image)
		lang := app.Get("language").String()
		if lang != "dockerfile" && checkIsRunner(image) {
			logrus.Debug("Discovered runner image at service: ", app.Get("service_cname"))
			isExist = true
			break
		}
	}
	if !isExist {
		logrus.Debug("Not discovered runner image in any service.")
		return nil
	}
	_, err = sources.ImagePull(i.DockerClient, image, builder.REGISTRYUSER, builder.REGISTRYPASS, i.Logger, 20)
	if err != nil {
		i.Logger.Error(fmt.Sprintf("Pull image failure：%s", image),
			map[string]string{"step": "pull-image", "status": "failure"})
		logrus.Error("Failed to pull image: ", err)
	}

	err = sources.ImageSave(i.DockerClient, image, fmt.Sprintf("%s/%s.image.tar", i.SourceDir, tarFileName), i.Logger)
	if err != nil {
		i.Logger.Error(fmt.Sprintf("Save image failure：%s", image),
			map[string]string{"step": "save-image", "status": "failure"})
		logrus.Error("Failed to save image: ", err)
		return err
	}
	logrus.Debug("Successful download runner image: ", image)
	return nil
}

func (i *ExportApp) saveComponentImages(images []string) error {
	logrus.Info("Start save component images")
	start := time.Now()
	ctx := context.Background()
	err := sources.MultiImageSave(ctx, i.DockerClient, fmt.Sprintf("%s/component-images.tar", i.SourceDir), i.Logger, images...)
	if err != nil {
		i.Logger.Error(fmt.Sprintf("Save image file failure"), map[string]string{"step": "save-image", "status": "failure"})
		logrus.Errorf("Failed to save image(%v) : %s", images, err)
		return err
	}
	logrus.Infof("Save component images success, Take %s time", time.Now().Sub(start))
	return nil
}

//DockerComposeYaml docker compose struct
type DockerComposeYaml struct {
	Version  string                  `yaml:"version"`
	Volumes  map[string]GlobalVolume `yaml:"volumes,omitempty"`
	Services map[string]*Service     `yaml:"services,omitempty"`
}

//Service service
type Service struct {
	Image         string            `yaml:"image"`
	ContainerName string            `yaml:"container_name,omitempty"`
	Restart       string            `yaml:"restart,omitempty"`
	NetworkMode   string            `yaml:"network_mode,omitempty"`
	Volumes       []string          `yaml:"volumes,omitempty"`
	Command       string            `yaml:"command,omitempty"`
	Environment   map[string]string `yaml:"environment,omitempty"`
	DependsOn     []string          `yaml:"depends_on,omitempty"`
	Loggin        struct {
		Driver  string `yaml:"driver,omitempty"`
		Options struct {
			MaxSize string `yaml:"max-size,omitempty"`
			MaxFile string `yaml:"max-file,omitempty"`
		}
	} `yaml:"logging,omitempty"`
}

// GlobalVolume is the volume for docker compose.
type GlobalVolume struct {
	External bool `yaml:"external"`
}

func (i *ExportApp) buildDockerComposeYaml() error {
	// Because updated the JSON file in the save Apps step, so need to reload it
	apps, err := i.parseApps()
	if err != nil {
		return err
	}

	y := &DockerComposeYaml{
		Version:  "2.1",
		Volumes:  make(map[string]GlobalVolume, 5),
		Services: make(map[string]*Service, 5),
	}

	i.Logger.Info("Start create docker compose app metadata file", map[string]string{"step": "build-yaml", "status": "starting"})
	logrus.Debug("Build docker compose yaml file in directory: ", i.SourceDir)

	ram, err := i.parseApp()
	if err != nil {
		return err
	}
	dockerCompose := newDockerCompose(ram)

	for _, app := range apps {
		shareImage := app.Get("share_image").String()
		shareUUID := app.Get("service_share_uuid").String()
		appName := dockerCompose.GetServiceName(shareUUID)

		volumes := dockerCompose.GetServiceVolumes(shareUUID)

		// environment variables
		envs := make(map[string]string, 10)
		if len(app.Get("port_map_list").Array()) > 0 {
			// The first port here maybe not as the same as the first one original
			port := app.Get("port_map_list").Array()[0]
			envs["PORT"] = port.Get("container_port").String()
		}
		envs["MEMORY_SIZE"] = envutil.GetMemoryType(int(app.Get("memory").Int()))
		for _, item := range app.Get("service_env_map_list").Array() {
			key := item.Get("attr_name").String()
			value := item.Get("attr_value").String()
			envs[key] = value
			if envs[key] == "**None**" {
				envs[key] = util.NewUUID()[:8]
			}
		}
		for _, item := range app.Get("service_connect_info_map_list").Array() {
			key := item.Get("attr_name").String()
			value := item.Get("attr_value").String()
			envs[key] = value
		}
		var depServices []string
		// 如果该app依赖了另了个app-b，则把app-b中所有公开环境变量注入到该app
		for _, item := range app.Get("dep_service_map_list").Array() {
			serviceKey := item.Get("dep_service_key").String()
			depEnvs := i.getPublicEnvByKey(serviceKey, &apps)
			for k, v := range depEnvs {
				envs[k] = v
			}

			if svc := i.getDependedService(serviceKey, &apps); svc != "" {
				depServices = append(depServices, composeName(svc))
			}
		}

		for key, value := range envs {
			// env rendering
			envs[key] = util.ParseVariable(value, envs)
		}

		service := &Service{
			Image:         sources.GenSaveImageName(shareImage),
			ContainerName: appName,
			Restart:       "always",
			NetworkMode:   "host",
			Volumes:       volumes,
			Command:       app.Get("cmd").String(),
			Environment:   envs,
		}
		service.Loggin.Driver = "json-file"
		service.Loggin.Options.MaxSize = "5m"
		service.Loggin.Options.MaxFile = "2"
		if len(depServices) > 0 {
			service.DependsOn = depServices
		}

		y.Services[appName] = service
	}

	y.Volumes = dockerCompose.GetGlobalVolumes()
	content, err := yaml.Marshal(y)
	if err != nil {
		i.Logger.Error(fmt.Sprintf("Create docker compose app metadata file failure：%v", err), map[string]string{"step": "build-yaml", "status": "failure"})
		logrus.Error("Failed to build yaml file: ", err)
		return err
	}

	err = ioutil.WriteFile(fmt.Sprintf("%s/docker-compose.yaml", i.SourceDir), content, 0644)
	if err != nil {
		i.Logger.Error(fmt.Sprintf("Create docker compose app metadata file failure：%v", err), map[string]string{"step": "create-yaml", "status": "failure"})
		logrus.Error("Failed to create yaml file: ", err)
		return err
	}
	i.Logger.Info("Create docker compose app metadata file success", map[string]string{"step": "build-yaml", "status": "success"})

	return nil
}

func (i *ExportApp) getPublicEnvByKey(serviceKey string, apps *[]gjson.Result) map[string]string {
	envs := make(map[string]string, 5)
	for _, app := range *apps {
		appKey := app.Get("service_share_uuid").String()
		if appKey == serviceKey {
			for _, item := range app.Get("service_connect_info_map_list").Array() {
				key := item.Get("attr_name").String()
				value := item.Get("attr_value").String()
				envs[key] = value
			}
			break
		}
	}

	return envs
}

func (i *ExportApp) getDependedService(key string, apps *[]gjson.Result) string {
	for _, app := range *apps {
		if key == app.Get("service_share_uuid").String() {
			return app.Get("service_cname").String()
		}
	}
	return ""
}

func (i *ExportApp) buildStartScript() error {
	if err := exec.Command("cp", "/src/export-app/run.sh", i.SourceDir).Run(); err != nil {
		err = errors.New("Failed to generate start script to: " + i.SourceDir)
		logrus.Error(err)
		return err
	}

	logrus.Debug("Successful generate start script to: ", i.SourceDir)
	return nil
}

//ErrorCallBack if run error will callback
func (i *ExportApp) ErrorCallBack(err error) {
	i.updateStatus("failed")
}

func (i *ExportApp) zip() error {
	err := util.Zip(i.SourceDir, i.SourceDir+".zip")
	if err != nil {
		i.Logger.Error("Export application failure:Zip failure", map[string]string{"step": "export-app", "status": "failure"})
		logrus.Errorf("Failed to create tar file for group %s: %v", i.SourceDir, err)
		return err
	}

	// create md5 file
	metadataFile := fmt.Sprintf("%s/metadata.json", i.SourceDir)
	if err := exec.Command("sh", "-c", fmt.Sprintf("md5sum %s > %s.md5", metadataFile, metadataFile)).Run(); err != nil {
		err = errors.New(fmt.Sprintf("Failed to create md5 file: %v", err))
		logrus.Error(err)
		return err
	}

	i.Logger.Info("Export application success", map[string]string{"step": "export-app", "status": "success"})
	logrus.Info("Successful export app by event id: ", i.EventID)
	return nil
}

func (i *ExportApp) updateStatus(status string) error {
	logrus.Debug("Update app status in database to: ", status)
	res, err := db.GetManager().AppDao().GetByEventId(i.EventID)
	if err != nil {
		err = errors.New(fmt.Sprintf("Failed to get app %s from db: %v", i.EventID, err))
		logrus.Error(err)
		return err
	}
	res.Status = status
	if err := db.GetManager().AppDao().UpdateModel(res); err != nil {
		err = errors.New(fmt.Sprintf("Failed to update app %s: %v", i.EventID, err))
		logrus.Error(err)
		return err
	}
	return nil
}

// 只保留"/"后面的部分，并去掉不合法字符，一般用于把镜像名变为将要导出的文件名
func buildToLinuxFileName(fileName string) string {
	if fileName == "" {
		return fileName
	}

	arr := strings.Split(fileName, "/")

	if str := arr[len(arr)-1]; str == "" {
		fileName = strings.Replace(fileName, "/", "---", -1)
	} else {
		fileName = str
	}

	fileName = strings.Replace(fileName, ":", "--", -1)
	fileName = re.ReplaceAllString(fileName, "")

	return fileName
}

type dockerCompose struct {
	ram            *ramv1alpha1.RainbondApplicationConfig
	globalVolumes  []string
	serviceVolumes map[string][]string
	serviceNames   map[string]string
}

func newDockerCompose(ram *ramv1alpha1.RainbondApplicationConfig) *dockerCompose {
	dc := &dockerCompose{
		ram: ram,
	}
	dc.build()
	return dc
}

func (d *dockerCompose) build() {
	// Important! serviceNames is always first
	d.serviceNames = d.buildServiceNames()
	d.serviceVolumes, d.globalVolumes = d.buildVolumes()
}

func (d *dockerCompose) buildServiceNames() map[string]string {
	names := make(map[string]string)
	set := make(map[string]struct{})
	for _, cpt := range d.ram.Components {
		name := composeName(cpt.ServiceCname)
		// make sure every name is unique
		if _, exists := set[name]; exists {
			name += "-" + util.NewUUID()[0:4]
		}
		set[name] = struct{}{}
		names[cpt.ServiceShareID] = name
	}
	return names
}

// build service volumes and global volumes
func (d *dockerCompose) buildVolumes() (map[string][]string, []string) {
	logrus.Debugf("start building volumes for %s", d.ram.AppName)

	var volumeMaps = make(map[string]string)
	var volumeList []string
	componentVolumes := make(map[string][]string)
	for _, cpt := range d.ram.Components {
		serviceName := d.GetServiceName(cpt.ServiceShareID)

		var volumes []string
		// own volumes
		for _, vol := range cpt.ServiceVolumeMapList {
			svolume, composeVolume, isConfig := d.buildVolume(serviceName, &vol)
			volumes = append(volumes, svolume)
			if composeVolume != "" {
				if !isConfig {
					volumeList = append(volumeList, composeVolume)
				}
				volumeMaps[cpt.ServiceShareID+vol.VolumeName] = composeVolume
			}
		}
		componentVolumes[cpt.ServiceShareID] = volumes
	}
	for _, cpt := range d.ram.Components {
		// dependent volumes
		for _, dvol := range cpt.MntReleationList {
			vol := volumeMaps[dvol.ShareServiceUUID+dvol.VolumeName]
			if vol == "" {
				logrus.Warningf("[dockerCompose] [buildVolumes] dependent volume(%s/%s) not found", dvol.ShareServiceUUID, dvol.VolumeName)
				continue
			}
			componentVolumes[cpt.ServiceShareID] = append(componentVolumes[cpt.ServiceShareID], fmt.Sprintf("%s:%s", vol, dvol.VolumeMountDir))
		}
	}
	return componentVolumes, volumeList
}

func (d *dockerCompose) buildVolume(serviceName string, volume *ramv1alpha1.ComponentVolume) (string, string, bool) {
	volumePath := volume.VolumeMountPath
	if volume.VolumeType == "config-file" {
		configFilePath := "./" + path.Join(serviceName, volume.VolumeMountPath)
		return fmt.Sprintf("%s:%s", configFilePath, volumePath), configFilePath, true
	}
	// make sure every volumeName is unique
	volumeName := serviceName + "_" + volume.VolumeName
	return fmt.Sprintf("%s:%s", volumeName, volumePath), volumeName, false
}

// GetServiceVolumes -
func (d *dockerCompose) GetServiceVolumes(shareServiceUUID string) []string {
	return d.serviceVolumes[shareServiceUUID]
}

// GetGlobalVolumes -
func (d *dockerCompose) GetGlobalVolumes() map[string]GlobalVolume {
	globalVolumes := make(map[string]GlobalVolume)
	for _, vol := range d.globalVolumes {
		globalVolumes[vol] = GlobalVolume{
			External: false,
		}
	}
	return globalVolumes
}

// GetServiceName -
func (d *dockerCompose) GetServiceName(shareServiceUUID string) string {
	return d.serviceNames[shareServiceUUID]
}

func findDepVolume(allVolumes map[string]ramv1alpha1.ComponentVolumeList, key, volumeName string) *ramv1alpha1.ComponentVolume {
	vols := allVolumes[key]
	// find related volume
	var volume *ramv1alpha1.ComponentVolume
	for _, vol := range vols {
		if vol.VolumeName == volumeName {
			volume = &vol
			break
		}
	}
	return volume
}
