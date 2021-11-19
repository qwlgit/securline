/*
Copyright 2021 The KodeRover Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package service

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"

	fsservice "github.com/koderover/zadig/pkg/microservice/aslan/core/common/service/fs"

	fsutil "github.com/koderover/zadig/pkg/util/fs"

	"github.com/hashicorp/go-multierror"

	cm "github.com/chartmuseum/helm-push/pkg/chartmuseum"
	"github.com/chartmuseum/helm-push/pkg/helm"
	"github.com/otiai10/copy"
	"github.com/pkg/errors"
	"go.uber.org/zap"
	chartloader "helm.sh/helm/v3/pkg/chart/loader"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/yaml"

	configbase "github.com/koderover/zadig/pkg/config"
	"github.com/koderover/zadig/pkg/microservice/aslan/config"
	commonmodels "github.com/koderover/zadig/pkg/microservice/aslan/core/common/repository/models"
	commonrepo "github.com/koderover/zadig/pkg/microservice/aslan/core/common/repository/mongodb"
	commonservice "github.com/koderover/zadig/pkg/microservice/aslan/core/common/service"
	"github.com/koderover/zadig/pkg/setting"
	e "github.com/koderover/zadig/pkg/tool/errors"
	"github.com/koderover/zadig/pkg/tool/log"
)

type CreateHelmDeliveryVersionOption struct {
	EnableOfflineDist bool   `json:"enableOfflineDist"`
	S3StorageID       string `json:"s3StorageID"`
}

type CreateHelmDeliveryVersionChartData struct {
	ServiceName       string `json:"serviceName"`
	Version           string `json:"version"`
	ValuesYamlContent string `json:"valuesYamlContent"`
}

type CreateHelmDeliveryVersionArgs struct {
	CreateBy    string   `json:"-"`
	Version     string   `json:"version"`
	Desc        string   `json:"desc"`
	ProductName string   `json:"productName"`
	EnvName     string   `json:"envName"`
	Labels      []string `json:"labels"`
	//ChartDatas    []*CreateHelmDeliveryVersionChartData `json:"chartDatas"`
	ChartRepoName string `json:"chartRepoName"`
	//Options       *CreateHelmDeliveryVersionOption      `json:"options"`
	*DeliveryVersionChartData
}

type DeliveryVersionChartData struct {
	ChartDatas []*CreateHelmDeliveryVersionChartData `json:"chartDatas"`
	Options    *CreateHelmDeliveryVersionOption      `json:"options"`
}

type ChartDeliveryData struct {
	ChartData      *CreateHelmDeliveryVersionChartData
	ProductService *commonmodels.ProductService
	ServiceObj     *commonmodels.Service
}

func GetDeliveryVersion(args *commonrepo.DeliveryVersionArgs, log *zap.SugaredLogger) (*commonmodels.DeliveryVersion, error) {
	resp, err := commonrepo.NewDeliveryVersionColl().Get(args)
	if err != nil {
		log.Errorf("get deliveryVersion error: %v", err)
		return nil, e.ErrGetDeliveryVersion
	}
	return resp, err
}

func FindDeliveryVersion(args *commonrepo.DeliveryVersionArgs, log *zap.SugaredLogger) ([]*commonmodels.DeliveryVersion, error) {
	resp, err := commonrepo.NewDeliveryVersionColl().Find(args)
	if err != nil {
		log.Errorf("find deliveryVersion error: %v", err)
		return resp, e.ErrFindDeliveryVersion
	}
	return resp, err
}

func DeleteDeliveryVersion(args *commonrepo.DeliveryVersionArgs, log *zap.SugaredLogger) error {
	err := commonrepo.NewDeliveryVersionColl().Delete(args.ID)
	if err != nil {
		log.Errorf("delete deliveryVersion error: %v", err)
		return e.ErrDeleteDeliveryVersion
	}
	return nil
}

func getProductEnvInfo(productName, envName string) (*commonmodels.Product, error) {
	productInfo, err := commonrepo.NewProductColl().Find(&commonrepo.ProductFindOptions{
		Name:    productName,
		EnvName: envName,
	})
	if err != nil {
		log.Errorf("failed to query product info, productName: %s envName: %s err: %s", productName, envName, err)
		return nil, fmt.Errorf("failed to query product info, productName: %s envName: %s", productName, envName)
	}
	return productInfo, nil
}

func getChartRepoData(repoName string) (*commonmodels.HelmRepo, error) {
	return commonrepo.NewHelmRepoColl().Find(&commonrepo.HelmRepoFindOption{RepoName: repoName})
}

func createChartRepoClient(repo *commonmodels.HelmRepo) (*cm.Client, error) {
	client, err := cm.NewClient(
		cm.URL(repo.URL),
		cm.Username(repo.Username),
		cm.Password(repo.Password),
		// need support more auth types
	)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to create chart repo client")
	}
	return client, nil
}

func handleSingleChart(chartData *ChartDeliveryData, chartRepo *commonmodels.HelmRepo, dir string) error {
	serviceObj := chartData.ServiceObj
	serviceName, revision := serviceObj.ServiceName, serviceObj.Revision
	base := config.LocalServicePathWithRevision(serviceObj.ProductName, serviceName, revision)
	if err := commonservice.PreloadServiceManifestsByRevision(base, serviceObj); err != nil {
		log.Warnf("failed to get chart of revision: %d for service: %s, use latest version", revision, serviceName)
		// use the latest version when it fails to download the specific version
		base = config.LocalServicePath(serviceObj.ProductName, serviceName)
		if err = commonservice.PreLoadServiceManifests(base, serviceObj); err != nil {
			log.Errorf("failed to load chart info for service %v", serviceObj.ServiceName)
			return err
		}
	}

	fullPath := filepath.Join(base, serviceObj.ServiceName)
	revisionBasePath := config.LocalDeliveryChartPathWithRevision(serviceObj.ProductName, serviceObj.ServiceName, serviceObj.Revision)
	deliveryChartPath := filepath.Join(revisionBasePath, serviceObj.ServiceName)
	err := copy.Copy(fullPath, deliveryChartPath)
	if err != nil {
		return nil
	}

	//load chart info from local storage
	chartRequested, err := chartloader.Load(deliveryChartPath)
	if err != nil {
		return errors.Wrapf(err, "failed to load chart info, path %s", deliveryChartPath)
	}

	//set version and values.yaml content
	chartRequested.Metadata.Name = chartData.ChartData.ServiceName
	chartRequested.Metadata.Version = chartData.ChartData.Version
	chartRequested.Metadata.AppVersion = chartData.ChartData.Version
	if len(chartData.ChartData.ValuesYamlContent) > 0 {
		valuesInfo := make(map[string]interface{})
		if err = yaml.Unmarshal([]byte(chartData.ChartData.ValuesYamlContent), map[string]interface{}{}); err != nil {
			log.Errorf("invalid yaml content, serviceName: %s, yamlContent: %s", serviceObj.ServiceName, chartData.ChartData.ValuesYamlContent)
			return errors.Wrapf(err, "invalid yaml content for service: %s", serviceObj.ServiceName)
		}
		chartRequested.Values = valuesInfo
	}

	chartPackagePath, err := helm.CreateChartPackage(&helm.Chart{Chart: chartRequested}, dir)
	if err != nil {
		return err
	}

	client, err := createChartRepoClient(chartRepo)
	if err != nil {
		return err
	}

	log.Infof("pushing %s to %s...\n", filepath.Base(chartPackagePath), chartRepo.URL)
	resp, err := client.UploadChartPackage(chartPackagePath, false)
	if err != nil {
		return errors.Wrapf(err, "failed to prepare pushing chart: %s", chartPackagePath)
	}
	err = handlePushResponse(resp)
	if err != nil {
		return errors.Wrapf(err, "failed to push chart: %s ", chartPackagePath)
	}
	return nil
}

func handlePushResponse(resp *http.Response) error {
	if resp.StatusCode != 201 && resp.StatusCode != 202 {
		b, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			return err
		}
		return getChartmuseumError(b, resp.StatusCode)
	}
	log.Infof("push chart to chart repo done")
	return nil
}

func getChartmuseumError(b []byte, code int) error {
	var er struct {
		Error string `json:"error"`
	}
	err := json.Unmarshal(b, &er)
	if err != nil || er.Error == "" {
		return errors.Errorf("%d: could not properly parse response JSON: %s", code, string(b))
	}
	return errors.Errorf("%d: %s", code, er.Error)
}

func mkChartTGZFileDir(versionName string) (string, error) {
	tmpDir := os.TempDir()
	path := filepath.Join(tmpDir, "chart-tgz", versionName)
	if err := os.RemoveAll(path); err != nil {
		if !os.IsExist(err) {
			return "", errors.Wrapf(err, "failed to claer dir for chart tgz files")
		}
	}
	err := os.MkdirAll(path, 0777)
	if err != nil {
		return "", errors.Wrapf(err, "failed to create chart tgz dir for version: %s", versionName)
	}
	return path, nil
}

func CreateHelmDeliveryVersion(args *CreateHelmDeliveryVersionArgs, logger *zap.SugaredLogger) error {
	// need appoint chart info
	if len(args.ChartDatas) == 0 {
		return e.ErrCreateDeliveryVersion.AddDesc("no chart info appointed")
	}

	// prepare data
	productInfo, err := getProductEnvInfo(args.ProductName, args.EnvName)
	if err != nil {
		log.Infof("failed to query product info, procutName: %s envName %s, err: %s", args.ProductName, args.EnvName, err)
		return e.ErrCreateDeliveryVersion.AddDesc(fmt.Sprintf("failed to query product info, procutName: %s envName %s", args.ProductName, args.EnvName))
	}
	repoInfo, err := getChartRepoData(args.ChartRepoName)
	if err != nil {
		log.Infof("failed to query chart-repo info, procutName: %s envName %s, err: %s", args.ProductName, args.EnvName, err)
		return e.ErrCreateDeliveryVersion.AddDesc(fmt.Sprintf("failed to query chart-repo info, procutName: %s envName %s", args.ProductName, args.EnvName))
	}

	dir, err := mkChartTGZFileDir(args.Version)
	if err != nil {
		return e.ErrCreateDeliveryVersion.AddErr(err)
	}

	// validate chartInfo, make sure service is in environment
	// prepare data set for chart delivery
	chartDataMap := make(map[string]*ChartDeliveryData)
	serviceMap := productInfo.GetServiceMap()
	for _, chartDta := range args.ChartDatas {
		if productService, ok := serviceMap[chartDta.ServiceName]; ok {
			serviceObj, err := commonrepo.NewServiceColl().Find(&commonrepo.ServiceFindOption{
				ServiceName: chartDta.ServiceName,
				Revision:    productService.Revision,
				Type:        setting.HelmDeployType,
				ProductName: args.ProductName,
			})
			if err != nil {
				return e.ErrCreateDeliveryVersion.AddDesc(fmt.Sprintf("failed to query service: %s", chartDta.ServiceName))
			}
			chartDataMap[chartDta.ServiceName] = &ChartDeliveryData{
				ChartData:      chartDta,
				ProductService: productService,
				ServiceObj:     serviceObj,
			}
		} else {
			return e.ErrCreateDeliveryVersion.AddDesc(fmt.Sprintf("service %s not found in environment", chartDta.ServiceName))
		}
	}

	productInfo.ID, _ = primitive.ObjectIDFromHex("")

	versionObj := &commonmodels.DeliveryVersion{
		Version:        args.Version,
		ProductName:    args.ProductName,
		Type:           setting.DeliveryVersionTypeChart,
		Desc:           args.Desc,
		Labels:         args.Labels,
		ProductEnvInfo: productInfo,
		Status:         setting.DeliveryVersionStatusCreating,
		CreateArgument: args.DeliveryVersionChartData,
		CreatedBy:      args.CreateBy,
		CreatedAt:      time.Now().Unix(),
		DeletedAt:      0,
	}

	err = commonrepo.NewDeliveryVersionColl().Insert(versionObj)
	if err != nil {
		logger.Errorf("failed to insert version data, err: %s", err)
		return fmt.Errorf("failed to insert delivery version: %s", versionObj.Version)
	}

	go func() {
		var err error
		defer func() {
			if err != nil {
				versionObj.Status = setting.DeliveryVersionStatusFailed
				versionObj.Error = err.Error()
			} else {
				versionObj.Status = setting.DeliveryVersionStatusSuccess
				versionObj.Error = ""
			}
			err = commonrepo.NewDeliveryVersionColl().UpdateStatusByName(versionObj.Version, versionObj.Status, versionObj.Error)
			if err != nil {
				logger.Errorf("failed to update delivery version data, name: %s error: %s", versionObj.Version, err)
			}
		}()

		var errLock sync.Mutex
		errorList := &multierror.Error{}

		appendError := func(err error) {
			errLock.Lock()
			defer errLock.Unlock()
			errorList = multierror.Append(errorList, err)
		}

		// push charts to repo
		wg := sync.WaitGroup{}
		for _, chartData := range chartDataMap {
			wg.Add(1)
			go func(cData *ChartDeliveryData) {
				defer wg.Done()
				err := handleSingleChart(cData, repoInfo, dir)
				if err != nil {
					logger.Errorf("failed to handle single chart data, serviceName: %s err: %s", cData.ChartData.ServiceName, err)
					appendError(err)
				} else {
					err = commonrepo.NewDeliveryVersionColl().AddDeliveryChart(versionObj.Version, &commonmodels.DeliveryChart{
						Name:    cData.ServiceObj.ServiceName,
						Version: cData.ChartData.Version,
						Repo:    args.ChartRepoName,
					})
					if err != nil {
						appendError(errors.Wrapf(err, "failed to save delivery chart: %s", cData.ChartData.ServiceName))
					}
				}
			}(chartData)
		}
		wg.Wait()

		if errorList.ErrorOrNil() != nil {
			err = errorList.ErrorOrNil()
			return
		}

		tmpDir, err := os.MkdirTemp("", "delivery-")
		if err != nil {
			logger.Errorf("failed to create temp dir, err: %s", err)
			return
		}
		defer os.RemoveAll(tmpDir)

		//tar all chart files and send to s3 store
		fsTree := os.DirFS(dir)
		tarball := fmt.Sprintf("%s.tar.gz", versionObj.Version)
		localPath := filepath.Join(tmpDir, tarball)
		if err = fsutil.Tar(fsTree, localPath); err != nil {
			logger.Errorf("failed to archive tarball %s, err: %s", localPath, err)
			versionObj.Status = setting.DeliveryVersionStatusFailed
			err = errors.Wrapf(err, "failed to archive chart files, path %s", localPath)
			return
		}

		ServiceS3Base := configbase.ObjectStorageDeliveryVersionPath(args.ProductName)
		if err = fsservice.ArchiveAndUploadFilesToS3(fsTree, versionObj.Version, ServiceS3Base, nil, logger); err != nil {
			logger.Errorf("failed to upload chart package files for project %s, err: %s", args.ProductName, err)
			err = errors.Wrapf(err, "failed to upload package file")
			return
		}

	}()

	return nil
}

func ListDeliveryServiceNames(productName string, log *zap.SugaredLogger) ([]string, error) {
	serviceNames := sets.String{}

	version := new(commonrepo.DeliveryVersionArgs)
	version.ProductName = productName
	deliveryVersions, err := FindDeliveryVersion(version, log)
	if err != nil {
		log.Errorf("FindDeliveryVersion failed, err:%v", err)
		return serviceNames.List(), err
	}

	for _, deliveryVersion := range deliveryVersions {
		deliveryDeployArgs := new(commonrepo.DeliveryDeployArgs)
		deliveryDeployArgs.ReleaseID = deliveryVersion.ID.Hex()
		deliveryDeploys, err := FindDeliveryDeploy(deliveryDeployArgs, log)
		if err != nil {
			log.Errorf("FindDeliveryDeploy failed, ReleaseID:%s, err:%v", deliveryVersion.ID, err)
			continue
		}
		for _, deliveryDeploy := range deliveryDeploys {
			serviceNames.Insert(deliveryDeploy.ServiceName)
		}
	}

	return serviceNames.UnsortedList(), nil
}
