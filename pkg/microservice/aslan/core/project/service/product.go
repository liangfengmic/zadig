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
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/hashicorp/go-multierror"
	"github.com/pkg/errors"
	"go.uber.org/zap"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/yaml"

	"github.com/koderover/zadig/pkg/microservice/aslan/config"
	commonmodels "github.com/koderover/zadig/pkg/microservice/aslan/core/common/repository/models"
	"github.com/koderover/zadig/pkg/microservice/aslan/core/common/repository/models/template"
	commonrepo "github.com/koderover/zadig/pkg/microservice/aslan/core/common/repository/mongodb"
	templaterepo "github.com/koderover/zadig/pkg/microservice/aslan/core/common/repository/mongodb/template"
	commonservice "github.com/koderover/zadig/pkg/microservice/aslan/core/common/service"
	"github.com/koderover/zadig/pkg/microservice/aslan/core/common/service/collie"
	environmentservice "github.com/koderover/zadig/pkg/microservice/aslan/core/environment/service"
	workflowservice "github.com/koderover/zadig/pkg/microservice/aslan/core/workflow/service/workflow"
	"github.com/koderover/zadig/pkg/setting"
	"github.com/koderover/zadig/pkg/shared/poetry"
	e "github.com/koderover/zadig/pkg/tool/errors"
	"github.com/koderover/zadig/pkg/tool/log"
	"github.com/koderover/zadig/pkg/types/permission"
)

type CustomParseDataArgs struct {
	Rules []*ImageParseData `json:"rules"`
}

type ImageParseData struct {
	Repo     string `json:"repo,omitempty"`
	Image    string `json:"image,omitempty"`
	Tag      string `json:"tag,omitempty"`
	InUse    bool   `json:"inUse,omitempty"`
	PresetId int    `json:"presetId,omitempty"`
}

func GetProductTemplateServices(productName string, log *zap.SugaredLogger) (*template.Product, error) {
	resp, err := templaterepo.NewProductColl().Find(productName)
	if err != nil {
		log.Errorf("GetProductTemplate error: %v", err)
		return nil, e.ErrGetProduct.AddDesc(err.Error())
	}

	err = FillProductTemplateVars([]*template.Product{resp}, log)
	if err != nil {
		return nil, fmt.Errorf("FillProductTemplateVars err : %v", err)
	}

	if resp.Services == nil {
		resp.Services = make([][]string, 0)
	}
	return resp, nil
}

// ListProductTemplate 列出产品模板分页
func ListProductTemplate(userID int, superUser bool, log *zap.SugaredLogger) ([]*template.Product, error) {
	var (
		err            error
		errorList      = &multierror.Error{}
		resp           = make([]*template.Product, 0)
		tmpls          = make([]*template.Product, 0)
		productTmpls   = make([]*template.Product, 0)
		productNameMap = make(map[string][]int64)
		productMap     = make(map[string]*template.Product)
		wg             sync.WaitGroup
		mu             sync.Mutex
		maxRoutineNum  = 20                            // 协程池最大协程数量
		ch             = make(chan int, maxRoutineNum) // 控制协程数量
	)

	poetryCtl := poetry.New(config.PoetryAPIServer(), config.PoetryAPIRootKey())

	tmpls, err = templaterepo.NewProductColl().List()
	if err != nil {
		log.Errorf("ProfuctTmpl.List error: %v", err)
		return resp, e.ErrListProducts.AddDesc(err.Error())
	}

	for _, product := range tmpls {
		if superUser {
			product.Role = setting.RoleAdmin
			product.PermissionUUIDs = []string{}
			product.ShowProject = true
			continue
		}
		productMap[product.ProductName] = product
	}

	if !superUser {
		productNameMap, err = poetryCtl.GetUserProject(userID, log)
		if err != nil {
			log.Errorf("ProfuctTmpl.List GetUserProject error: %v", err)
			return resp, e.ErrListProducts.AddDesc(err.Error())
		}

		// 优先处理客户有明确关联关系的项目
		for productName, roleIDs := range productNameMap {
			wg.Add(1)
			ch <- 1
			// 临时复制range获取的数据，避免重复操作最后一条数据
			tmpProductName := productName
			tmpRoleIDs := roleIDs

			go func(tmpProductName string, tmpRoleIDs []int64) {
				defer func() {
					<-ch
					wg.Done()
				}()

				roleID := tmpRoleIDs[0]
				product, err := templaterepo.NewProductColl().Find(tmpProductName)
				if err != nil {
					errorList = multierror.Append(errorList, err)
					log.Errorf("ProfuctTmpl.List error: %v", err)
					return
				}
				uuids, err := poetryCtl.GetUserPermissionUUIDs(roleID, tmpProductName, log)
				if err != nil {
					errorList = multierror.Append(errorList, err)
					log.Errorf("ProfuctTmpl.List GetUserPermissionUUIDs error: %v", err)
					return
				}
				if roleID == setting.RoleOwnerID {
					product.Role = setting.RoleOwner
					product.PermissionUUIDs = []string{}
				} else {
					product.Role = setting.RoleUser
					product.PermissionUUIDs = uuids
				}
				product.ShowProject = true
				mu.Lock()
				productTmpls = append(productTmpls, product)
				delete(productMap, tmpProductName)
				mu.Unlock()
			}(tmpProductName, tmpRoleIDs)
		}

		wg.Wait()
		if errorList.ErrorOrNil() != nil {
			return resp, errorList
		}

		// 增加项目里面设置过all-users的权限处理
		for _, product := range productMap {
			wg.Add(1)
			ch <- 1
			// 临时复制range获取的数据，避免重复操作最后一条数据
			tmpProduct := product

			go func(tmpProduct *template.Product) {
				defer func() {
					<-ch
					wg.Done()
				}()

				productRole, _ := poetryCtl.ListRoles(tmpProduct.ProductName, log)
				if productRole != nil {
					uuids, err := poetryCtl.GetUserPermissionUUIDs(productRole.ID, tmpProduct.ProductName, log)
					if err != nil {
						errorList = multierror.Append(errorList, err)
						log.Errorf("ProfuctTmpl.List GetUserPermissionUUIDs error: %v", err)
						return
					}

					tmpProduct.Role = setting.RoleUser
					tmpProduct.PermissionUUIDs = uuids
					tmpProduct.ShowProject = true
					mu.Lock()
					productTmpls = append(productTmpls, tmpProduct)
					delete(productMap, tmpProduct.ProductName)
					mu.Unlock()
				}
			}(tmpProduct)
		}
		wg.Wait()
		if errorList.ErrorOrNil() != nil {
			return resp, errorList
		}

		// 最后处理剩余的项目
		for _, product := range productMap {
			wg.Add(1)
			ch <- 1
			// 临时复制range获取的数据，避免重复操作最后一条数据
			tmpProduct := product

			go func(tmpProduct *template.Product) {
				defer func() {
					<-ch
					wg.Done()
				}()

				var uuids []string
				uuids, err = poetryCtl.GetUserPermissionUUIDs(setting.RoleUserID, "", log)
				if err != nil {
					errorList = multierror.Append(errorList, err)
					log.Errorf("ProfuctTmpl.List GetUserPermissionUUIDs error: %v", err)
					return
				}
				tmpProduct.Role = setting.RoleUser
				tmpProduct.PermissionUUIDs = uuids
				tmpProduct.ShowProject = false
				mu.Lock()
				productTmpls = append(productTmpls, tmpProduct)
				mu.Unlock()
			}(tmpProduct)
		}
		wg.Wait()
		if errorList.ErrorOrNil() != nil {
			return resp, errorList
		}
		// 先清空tmpls中的管理员角色数据后，再插入普通用户角色的数据
		tmpls = make([]*template.Product, 0)
		tmpls = append(tmpls, productTmpls...)
	}

	err = FillProductTemplateVars(tmpls, log)
	if err != nil {
		return resp, err
	}

	for _, tmpl := range tmpls {
		wg.Add(1)
		ch <- 1

		go func(tmpTmpl *template.Product) {
			defer func() {
				<-ch
				wg.Done()
			}()

			tmpTmpl.TotalServiceNum, err = commonrepo.NewServiceColl().Count(tmpTmpl.ProductName)
			if err != nil {
				errorList = multierror.Append(errorList, err)
				return
			}

			tmpTmpl.TotalEnvNum, err = commonrepo.NewProductColl().Count(tmpTmpl.ProductName)
			if err != nil {
				errorList = multierror.Append(errorList, err)
				return
			}

			mu.Lock()
			resp = append(resp, tmpTmpl)
			mu.Unlock()
		}(tmpl)
	}
	wg.Wait()
	if errorList.ErrorOrNil() != nil {
		return resp, errorList
	}

	return resp, nil
}

func ListOpenSourceProduct(log *zap.SugaredLogger) ([]*template.Product, error) {
	opt := &templaterepo.ProductListOpt{
		IsOpensource: "true",
	}

	tmpls, err := templaterepo.NewProductColl().ListWithOption(opt)
	if err != nil {
		log.Errorf("ProductTmpl.ListWithOpt error: %v", err)
		return nil, e.ErrListProducts.AddDesc(err.Error())
	}

	return tmpls, nil
}

// CreateProductTemplate 创建产品模板
func CreateProductTemplate(args *template.Product, log *zap.SugaredLogger) (err error) {
	kvs := args.Vars
	// 不保存vas
	args.Vars = nil

	err = commonservice.ValidateKVs(kvs, args.AllServiceInfos(), log)
	if err != nil {
		return e.ErrCreateProduct.AddDesc(err.Error())
	}

	if err := ensureProductTmpl(args); err != nil {
		return e.ErrCreateProduct.AddDesc(err.Error())
	}

	err = templaterepo.NewProductColl().Create(args)
	if err != nil {
		log.Errorf("ProductTmpl.Create error: %v", err)
		return e.ErrCreateProduct.AddDesc(err.Error())
	}

	// 创建一个默认的渲染集
	err = commonservice.CreateRenderSet(&commonmodels.RenderSet{
		Name:        args.ProductName,
		ProductTmpl: args.ProductName,
		UpdateBy:    args.UpdateBy,
		IsDefault:   true,
		KVs:         kvs,
	}, log)

	if err != nil {
		log.Errorf("ProductTmpl.Create error: %v", err)
		// 创建渲染集失败，删除产品模板
		return e.ErrCreateProduct.AddDesc(err.Error())
	}

	return
}

// UpdateProductTemplate 更新产品模板
func UpdateProductTemplate(name string, args *template.Product, log *zap.SugaredLogger) (err error) {
	kvs := args.Vars
	args.Vars = nil

	if err = commonservice.ValidateKVs(kvs, args.AllServiceInfos(), log); err != nil {
		log.Warnf("ProductTmpl.Update ValidateKVs error: %v", err)
	}

	if err := ensureProductTmpl(args); err != nil {
		return e.ErrUpdateProduct.AddDesc(err.Error())
	}

	if err = templaterepo.NewProductColl().Update(name, args); err != nil {
		log.Errorf("ProductTmpl.Update error: %v", err)
		return e.ErrUpdateProduct
	}
	// 如果是helm的项目，不需要新创建renderset
	if args.ProductFeature != nil && args.ProductFeature.DeployType == setting.HelmDeployType {
		return
	}
	// 更新默认的渲染集
	if err = commonservice.CreateRenderSet(&commonmodels.RenderSet{
		Name:        args.ProductName,
		ProductTmpl: args.ProductName,
		UpdateBy:    args.UpdateBy,
		IsDefault:   true,
		KVs:         kvs,
	}, log); err != nil {
		log.Warnf("ProductTmpl.Update CreateRenderSet error: %v", err)
	}

	for _, envVars := range args.EnvVars {
		//创建集成环境变量
		if err = commonservice.CreateRenderSet(&commonmodels.RenderSet{
			EnvName:     envVars.EnvName,
			Name:        args.ProductName,
			ProductTmpl: args.ProductName,
			UpdateBy:    args.UpdateBy,
			IsDefault:   false,
			KVs:         envVars.Vars,
		}, log); err != nil {
			log.Warnf("ProductTmpl.Update CreateRenderSet error: %v", err)
		}
	}

	// 更新子环境渲染集
	if err = commonservice.UpdateSubRenderSet(args.ProductName, kvs, log); err != nil {
		log.Warnf("ProductTmpl.Update UpdateSubRenderSet error: %v", err)
	}

	return nil
}

// UpdateProductTmplStatus 更新项目onboarding状态
func UpdateProductTmplStatus(productName, onboardingStatus string, log *zap.SugaredLogger) (err error) {
	status, err := strconv.Atoi(onboardingStatus)
	if err != nil {
		log.Errorf("convert onboardingStatus to int failed, error: %v", err)
		return e.ErrUpdateProduct.AddErr(err)
	}

	if err = templaterepo.NewProductColl().UpdateOnboardingStatus(productName, status); err != nil {
		log.Errorf("ProductTmpl.UpdateOnboardingStatus failed, productName:%s, status:%d, error: %v", productName, status, err)
		return e.ErrUpdateProduct.AddErr(err)
	}

	return nil
}

func UpdateServiceOrder(username, name string, services [][]string, log *zap.SugaredLogger) error {
	if err := templaterepo.NewProductColl().UpdateServiceOrder(&template.ProductArgs{
		ProductName: name,
		Services:    services,
		UpdateBy:    username,
	}); err != nil {
		log.Errorf("failed to update service order,err:%s", err)
		return e.ErrUpdateProduct.AddDesc("failed to update service order")
	}
	return nil
}

// UpdateProject 更新项目
func UpdateProject(name string, args *template.Product, log *zap.SugaredLogger) (err error) {
	err = validateRule(args.CustomImageRule, args.CustomTarRule)
	if err != nil {
		return e.ErrInvalidParam.AddDesc(err.Error())
	}
	poetryCtl := poetry.New(config.PoetryAPIServer(), config.PoetryAPIRootKey())
	//创建团建和项目之间的关系
	_, err = poetryCtl.AddProductTeam(args.ProductName, args.TeamID, args.UserIDs, log)
	if err != nil {
		log.Errorf("Project.Create AddProductTeam error: %v", err)
		return e.ErrUpdateProduct.AddDesc(err.Error())
	}

	err = templaterepo.NewProductColl().Update(name, args)
	if err != nil {
		log.Errorf("Project.Update error: %v", err)
		return e.ErrUpdateProduct.AddDesc(err.Error())
	}
	return nil
}

func validateRule(customImageRule *template.CustomRule, customTarRule *template.CustomRule) error {
	var (
		customImageRuleMap map[string]string
		customTarRuleMap   map[string]string
	)
	body, err := json.Marshal(&customImageRule)
	if err != nil {
		return err
	}
	if err = json.Unmarshal(body, &customImageRuleMap); err != nil {
		return err
	}

	for field, ruleValue := range customImageRuleMap {
		if err := validateCommonRule(ruleValue, field, config.ImageResourceType); err != nil {
			return err
		}
	}

	body, err = json.Marshal(&customTarRule)
	if err != nil {
		return err
	}
	if err = json.Unmarshal(body, &customTarRuleMap); err != nil {
		return err
	}
	for field, ruleValue := range customTarRuleMap {
		if err := validateCommonRule(ruleValue, field, config.TarResourceType); err != nil {
			return err
		}
	}

	return nil
}

func validateCommonRule(currentRule, ruleType, deliveryType string) error {
	var (
		imageRegexString = "^[a-z0-9][a-zA-Z0-9-_:.]+$"
		tarRegexString   = "^[a-z0-9][a-zA-Z0-9-_.]+$"
		tagRegexString   = "^[a-z0-9A-Z_][a-zA-Z0-9-_.]+$"
		errMessage       = "contains invalid characters, please check"
	)

	if currentRule == "" {
		return fmt.Errorf("%s can not be empty", ruleType)
	}

	if deliveryType == config.ImageResourceType && !strings.Contains(currentRule, ":") {
		return fmt.Errorf("%s is invalid, must contain a colon", ruleType)
	}

	currentRule = commonservice.ReplaceRuleVariable(currentRule, &commonservice.Variable{
		"ss", "ss", "ss", "ss", "ss", "ss", "ss", "ss", "ss",
	})
	switch deliveryType {
	case config.ImageResourceType:
		if !regexp.MustCompile(imageRegexString).MatchString(currentRule) {
			return fmt.Errorf("image %s %s", ruleType, errMessage)
		}
		// validate tag
		tag := strings.Split(currentRule, ":")[1]
		if !regexp.MustCompile(tagRegexString).MatchString(tag) {
			return fmt.Errorf("image %s %s", ruleType, errMessage)
		}
	case config.TarResourceType:
		if !regexp.MustCompile(tarRegexString).MatchString(currentRule) {
			return fmt.Errorf("tar %s %s", ruleType, errMessage)
		}
	}
	return nil
}

// DeleteProductTemplate 删除产品模板
func DeleteProductTemplate(userName, productName, requestID string, log *zap.SugaredLogger) (err error) {
	publicServices, err := commonrepo.NewServiceColl().ListMaxRevisions(&commonrepo.ServiceListOption{ProductName: productName, Visibility: setting.PublicService})
	if err != nil {
		log.Errorf("pre delete check failed, err: %s", err)
		return e.ErrDeleteProduct.AddDesc(err.Error())
	}

	serviceToProject, err := commonservice.GetServiceInvolvedProjects(publicServices, productName)
	if err != nil {
		log.Errorf("pre delete check failed, err: %s", err)
		return e.ErrDeleteProduct.AddDesc(err.Error())
	}
	for k, v := range serviceToProject {
		if len(v) > 0 {
			return e.ErrDeleteProduct.AddDesc(fmt.Sprintf("共享服务[%s]在项目%v中被引用，请解除引用后删除", k, v))
		}
	}

	poetryCtl := poetry.New(config.PoetryAPIServer(), config.PoetryAPIRootKey())

	//删除项目团队信息
	if err = poetryCtl.DeleteProductTeam(productName, log); err != nil {
		log.Errorf("productTeam.Delete error: %v", err)
		return e.ErrDeleteProduct
	}

	envs, _ := commonrepo.NewProductColl().List(&commonrepo.ProductListOptions{Name: productName})
	for _, env := range envs {
		if err = commonrepo.NewProductColl().UpdateStatus(env.EnvName, productName, setting.ProductStatusDeleting); err != nil {
			log.Errorf("DeleteProductTemplate Update product Status error: %v", err)
			return e.ErrDeleteProduct
		}
	}

	if err = commonservice.DeleteRenderSet(productName, log); err != nil {
		log.Errorf("DeleteProductTemplate DeleteRenderSet err: %v", err)
		return err
	}

	if err = DeleteTestModules(productName, requestID, log); err != nil {
		log.Errorf("DeleteProductTemplate Delete productName %s test err: %v", productName, err)
		return err
	}

	if err = commonservice.DeleteWorkflows(productName, requestID, log); err != nil {
		log.Errorf("DeleteProductTemplate Delete productName %s workflow err: %v", productName, err)
		return err
	}

	if err = commonservice.DeletePipelines(productName, requestID, log); err != nil {
		log.Errorf("DeleteProductTemplate Delete productName %s pipeline err: %v", productName, err)
		return err
	}

	//删除自由编排工作流
	features, err := commonservice.GetFeatures(log)
	if err != nil {
		log.Errorf("DeleteProductTemplate productName %s getFeatures err: %v", productName, err)
	}
	if strings.Contains(features, string(config.FreestyleType)) {
		collieClient := collie.New(config.CollieAPIAddress(), config.PoetryAPIRootKey())
		if err = collieClient.DeleteCIPipelines(productName, log); err != nil {
			log.Errorf("DeleteProductTemplate Delete productName %s freestyle pipeline err: %v", productName, err)
		}
	}

	err = templaterepo.NewProductColl().Delete(productName)
	if err != nil {
		log.Errorf("ProductTmpl.Delete error: %v", err)
		return e.ErrDeleteProduct
	}

	err = commonrepo.NewCounterColl().Delete(fmt.Sprintf("product:%s", productName))
	if err != nil {
		log.Errorf("Counter.Delete error: %v", err)
		return err
	}

	services, _ := commonrepo.NewServiceColl().ListMaxRevisions(
		&commonrepo.ServiceListOption{ProductName: productName, Type: setting.K8SDeployType},
	)
	for _, s := range services {
		commonservice.ProcessServiceWebhook(nil, s, s.ServiceName, log)
	}

	//删除交付中心
	//删除构建/删除测试/删除服务
	//删除workflow和历史task
	go func() {
		_ = commonrepo.NewBuildColl().Delete("", productName)
		_ = commonrepo.NewServiceColl().Delete("", "", productName, "", 0)
		_ = commonservice.DeleteDeliveryInfos(productName, log)
		_ = DeleteProductsAsync(userName, productName, requestID, log)
	}()
	// 删除workload
	go func() {
		workloads, _ := commonrepo.NewWorkLoadsStatColl().FindByProductName(productName)
		for _, v := range workloads {
			// update workloads
			tmp := []commonmodels.Workload{}
			for _, vv := range v.Workloads {
				if vv.ProductName != productName {
					tmp = append(tmp, vv)
				}
			}
			v.Workloads = tmp
			commonrepo.NewWorkLoadsStatColl().UpdateWorkloads(v)
		}
	}()
	// delete servicesInExternalEnv data
	go func() {
		_ = commonrepo.NewServicesInExternalEnvColl().Delete(&commonrepo.ServicesInExternalEnvArgs{
			ProductName: productName,
		})
	}()
	return nil
}

func ForkProduct(userID int, username, requestID string, args *template.ForkProject, log *zap.SugaredLogger) error {
	poetryClient := poetry.New(config.PoetryAPIServer(), config.PoetryAPIRootKey())
	// first check if the product have contributor role, if not, create one
	if !poetryClient.ContributorRoleExist(args.ProductName, log) {
		err := poetryClient.CreateContributorRole(args.ProductName, log)
		if err != nil {
			log.Errorf("Cannot create contributor role for product: %s, the error is: %v", args.ProductName, err)
			return e.ErrForkProduct.AddDesc(err.Error())
		}
	}

	// Give contributor role to this user
	// first look for roleID
	roleID := poetryClient.GetContributorRoleID(args.ProductName, log)
	if roleID < 0 {
		log.Errorf("Failed to get contributor Role ID from poetry client")
		return e.ErrForkProduct.AddDesc("Failed to get contributor Role ID from poetry client")
	}

	prodTmpl, err := templaterepo.NewProductColl().Find(args.ProductName)
	if err != nil {
		errMsg := fmt.Sprintf("[ProductTmpl.Find] %s error: %v", args.ProductName, err)
		log.Error(errMsg)
		return e.ErrForkProduct.AddDesc(errMsg)
	}

	prodTmpl.ChartInfos = args.ValuesYamls
	// Load Service
	var svcs [][]*commonmodels.ProductService
	allServiceInfoMap := prodTmpl.AllServiceInfoMap()
	for _, names := range prodTmpl.Services {
		servicesResp := make([]*commonmodels.ProductService, 0)

		for _, serviceName := range names {
			opt := &commonrepo.ServiceFindOption{
				ServiceName:   serviceName,
				ProductName:   allServiceInfoMap[serviceName].Owner,
				ExcludeStatus: setting.ProductStatusDeleting,
			}

			serviceTmpl, err := commonrepo.NewServiceColl().Find(opt)
			if err != nil {
				errMsg := fmt.Sprintf("[ServiceTmpl.List] %s error: %v", opt.ServiceName, err)
				log.Error(errMsg)
				return e.ErrForkProduct.AddDesc(errMsg)
			}
			serviceResp := &commonmodels.ProductService{
				ServiceName: serviceTmpl.ServiceName,
				ProductName: serviceTmpl.ProductName,
				Type:        serviceTmpl.Type,
				Revision:    serviceTmpl.Revision,
			}
			if serviceTmpl.Type == setting.HelmDeployType {
				serviceResp.Containers = make([]*commonmodels.Container, 0)
				for _, c := range serviceTmpl.Containers {
					container := &commonmodels.Container{
						Name:      c.Name,
						Image:     c.Image,
						ImagePath: c.ImagePath,
					}
					serviceResp.Containers = append(serviceResp.Containers, container)
				}
			}
			servicesResp = append(servicesResp, serviceResp)
		}
		svcs = append(svcs, servicesResp)
	}

	prod := commonmodels.Product{
		ProductName:     prodTmpl.ProductName,
		Revision:        prodTmpl.Revision,
		IsPublic:        false,
		EnvName:         args.EnvName,
		Services:        svcs,
		Source:          setting.HelmDeployType,
		ChartInfos:      prodTmpl.ChartInfos,
		IsForkedProduct: true,
	}

	err = environmentservice.CreateProduct(username, requestID, &prod, log)
	if err != nil {
		_, messageMap := e.ErrorMessage(err)
		if description, ok := messageMap["description"]; ok {
			return e.ErrForkProduct.AddDesc(description.(string))
		}
		errMsg := fmt.Sprintf("Failed to create env in order to fork product, the error is: %+v", err)
		log.Errorf(errMsg)
		return e.ErrForkProduct.AddDesc(errMsg)
	}

	userList, _ := poetryClient.ListPermissionUsers(args.ProductName, roleID, poetry.ProjectType, log)
	newUserList := append(userList, userID)
	err = poetryClient.UpdateUserRole(roleID, poetry.ProjectType, args.ProductName, newUserList, log)
	if err != nil {
		log.Errorf("Failed to update user role, the error is: %v", err)
		return e.ErrForkProduct.AddDesc(fmt.Sprintf("Failed to update user role, the error is: %v", err))
	}

	err = poetryClient.CreateUserEnvPermission(&poetry.UserEnvPermission{
		UserID:          userID,
		ProductName:     args.ProductName,
		EnvName:         args.EnvName,
		PermissionUUIDs: []string{permission.TestEnvListUUID, permission.TestEnvManageUUID},
	})
	if err != nil {
		return e.ErrForkProduct.AddDesc(fmt.Sprintf("Failed to create env permission for user: %s", username))
	}

	workflowPreset, err := workflowservice.PreSetWorkflow(args.ProductName, log)
	if err != nil {
		errMsg := fmt.Sprintf("Failed to get workflow preset info, the error is: %+v", err)
		log.Error(errMsg)
		return e.ErrForkProduct.AddDesc(errMsg)
	}

	buildModule := []*commonmodels.BuildModule{}
	artifactModule := []*commonmodels.ArtifactModule{}
	for _, i := range workflowPreset {
		buildModuleVer := "stable"
		if len(i.BuildModuleVers) != 0 {
			buildModuleVer = i.BuildModuleVers[0]
		}
		buildModule = append(buildModule, &commonmodels.BuildModule{
			BuildModuleVer: buildModuleVer,
			Target:         i.Target,
		})
		artifactModule = append(artifactModule, &commonmodels.ArtifactModule{Target: i.Target})
	}

	workflowArgs := &commonmodels.Workflow{
		ArtifactStage:   &commonmodels.ArtifactStage{Enabled: true, Modules: artifactModule},
		BuildStage:      &commonmodels.BuildStage{Enabled: false, Modules: buildModule},
		Name:            args.WorkflowName,
		ProductTmplName: args.ProductName,
		Enabled:         true,
		EnvName:         args.EnvName,
		TestStage:       &commonmodels.TestStage{Enabled: false, Tests: []*commonmodels.TestExecArgs{}},
		SecurityStage:   &commonmodels.SecurityStage{Enabled: false},
		DistributeStage: &commonmodels.DistributeStage{
			Enabled:     false,
			Distributes: []*commonmodels.ProductDistribute{},
			Releases:    []commonmodels.RepoImage{},
		},
		HookCtl:   &commonmodels.WorkflowHookCtrl{Enabled: false, Items: []*commonmodels.WorkflowHook{}},
		Schedules: &commonmodels.ScheduleCtrl{Enabled: false, Items: []*commonmodels.Schedule{}},
		CreateBy:  username,
		UpdateBy:  username,
	}

	return workflowservice.CreateWorkflow(workflowArgs, log)
}

func UnForkProduct(userID int, username, productName, workflowName, envName, requestID string, log *zap.SugaredLogger) error {
	poetryClient := poetry.New(config.PoetryAPIServer(), config.PoetryAPIRootKey())
	if userEnvPermissions, _ := poetryClient.ListUserEnvPermission(productName, userID, log); len(userEnvPermissions) > 0 {
		if err := poetryClient.DeleteUserEnvPermission(productName, username, userID, log); err != nil {
			return e.ErrUnForkProduct.AddDesc(fmt.Sprintf("Failed to delete env permission for userID: %d, env: %s, productName: %s, the error is: %+v", userID, username, productName, err))
		}
	}

	if _, err := workflowservice.FindWorkflow(workflowName, log); err == nil {
		err = commonservice.DeleteWorkflow(workflowName, requestID, false, log)
		if err != nil {
			log.Errorf("Failed to delete forked workflow: %s, the error is: %+v", workflowName, err)
			return e.ErrUnForkProduct.AddDesc(err.Error())
		}
	}

	if roleID := poetryClient.GetContributorRoleID(productName, log); roleID > 0 {
		err := poetryClient.DeleteUserRole(roleID, poetry.ProjectType, userID, productName, log)
		if err != nil {
			log.Errorf("Failed to Delete user from role candidate, the error is: %v", err)
			return e.ErrUnForkProduct.AddDesc(err.Error())
		}
	}

	if err := commonservice.DeleteProduct(username, envName, productName, requestID, log); err != nil {
		_, messageMap := e.ErrorMessage(err)
		if description, ok := messageMap["description"]; ok {
			if description != "not found" {
				return e.ErrUnForkProduct.AddDesc(description.(string))
			}
		} else {
			errMsg := fmt.Sprintf("Failed to delete env %s in order to unfork product, the error is: %+v", envName, err)
			log.Errorf(errMsg)
			return e.ErrUnForkProduct.AddDesc(errMsg)
		}
	}
	return nil
}

func FillProductTemplateVars(productTemplates []*template.Product, log *zap.SugaredLogger) error {
	return commonservice.FillProductTemplateVars(productTemplates, log)
}

// ensureProductTmpl 检查产品模板参数
func ensureProductTmpl(args *template.Product) error {
	if args == nil {
		return errors.New("nil ProductTmpl")
	}

	if len(args.ProductName) == 0 {
		return errors.New("empty product name")
	}

	if !config.ServiceNameRegex.MatchString(args.ProductName) {
		return fmt.Errorf("product name must match %s", config.ServiceNameRegexString)
	}

	serviceNames := sets.NewString()
	for _, sg := range args.Services {
		for _, s := range sg {
			if serviceNames.Has(s) {
				return fmt.Errorf("duplicated service found: %s", s)
			}
			serviceNames.Insert(s)
		}
	}

	// Revision为0表示是新增项目，新增项目不需要进行共享服务的判断，只在编辑项目时进行判断
	if args.Revision != 0 {
		//获取该项目下的所有服务
		productTmpl, err := templaterepo.NewProductColl().Find(args.ProductName)
		if err != nil {
			log.Errorf("Can not find project %s, error: %s", args.ProductName, err)
			return fmt.Errorf("project not found: %s", err)
		}

		var newSharedServices []*template.ServiceInfo
		currentSharedServiceMap := productTmpl.SharedServiceInfoMap()
		for _, s := range args.SharedServices {
			if _, ok := currentSharedServiceMap[s.Name]; !ok {
				newSharedServices = append(newSharedServices, s)
			}
		}

		if len(newSharedServices) > 0 {
			services, err := commonrepo.NewServiceColl().ListMaxRevisions(&commonrepo.ServiceListOption{
				InServices: newSharedServices,
				Visibility: setting.PublicService,
			})
			if err != nil {
				log.Errorf("Failed to list services, err: %s", err)
				return err
			}

			if len(newSharedServices) != len(services) {
				return fmt.Errorf("新增的共享服务服务不存在或者已经不是共享服务")
			}
		}
	}

	// 设置新的版本号
	rev, err := commonrepo.NewCounterColl().GetNextSeq("product:" + args.ProductName)
	if err != nil {
		return fmt.Errorf("get next product template revision error: %v", err)
	}

	args.Revision = rev
	return nil
}

func DeleteProductsAsync(userName, productName, requestID string, log *zap.SugaredLogger) error {
	envs, err := commonrepo.NewProductColl().List(&commonrepo.ProductListOptions{Name: productName})
	if err != nil {
		return e.ErrListProducts.AddDesc(err.Error())
	}
	errList := new(multierror.Error)
	for _, env := range envs {
		err = commonservice.DeleteProduct(userName, env.EnvName, productName, requestID, log)
		if err != nil {
			errList = multierror.Append(errList, err)
		}
	}
	if err := errList.ErrorOrNil(); err != nil {
		log.Errorf("DeleteProductsAsync err:%v", err)
		return err
	}
	return nil
}

type ProductInfo struct {
	Value       string         `bson:"value"              json:"value"`
	Label       string         `bson:"label"              json:"label"`
	ServiceInfo []*ServiceInfo `bson:"services"           json:"services"`
}

type ServiceInfo struct {
	Value         string           `bson:"value"              json:"value"`
	Label         string           `bson:"label"              json:"label"`
	ContainerInfo []*ContainerInfo `bson:"containers"         json:"containers"`
}

// ContainerInfo ...
type ContainerInfo struct {
	Value string `bson:"value"              json:"value"`
	Label string `bson:"label"              json:"label"`
}

func ListTemplatesHierachy(userName string, userID int, superUser bool, log *zap.SugaredLogger) ([]*ProductInfo, error) {
	var (
		err          error
		resp         = make([]*ProductInfo, 0)
		productTmpls = make([]*template.Product, 0)
	)

	if superUser {
		productTmpls, err = templaterepo.NewProductColl().List()
		if err != nil {
			log.Errorf("[%s] ProductTmpl.List error: %v", userName, err)
			return nil, e.ErrListProducts.AddDesc(err.Error())
		}
	} else {
		productNameMap, err := poetry.New(config.PoetryAPIServer(), config.PoetryAPIRootKey()).GetUserProject(userID, log)
		if err != nil {
			log.Errorf("ProfuctTmpl.List GetUserProject error: %v", err)
			return resp, e.ErrListProducts.AddDesc(err.Error())
		}
		for productName := range productNameMap {
			product, err := templaterepo.NewProductColl().Find(productName)
			if err != nil {
				log.Errorf("ProfuctTmpl.List error: %v", err)
				return resp, e.ErrListProducts.AddDesc(err.Error())
			}
			productTmpls = append(productTmpls, product)
		}
	}

	for _, productTmpl := range productTmpls {
		pInfo := &ProductInfo{Value: productTmpl.ProductName, Label: productTmpl.ProductName, ServiceInfo: []*ServiceInfo{}}
		services, err := commonrepo.NewServiceColl().ListMaxRevisionsForServices(productTmpl.AllServiceInfos(), "")
		if err != nil {
			log.Errorf("Failed to list service for project %s, error: %s", productTmpl.ProductName, err)
			return nil, e.ErrGetProduct.AddDesc(err.Error())
		}
		for _, svcTmpl := range services {
			sInfo := &ServiceInfo{Value: svcTmpl.ServiceName, Label: svcTmpl.ServiceName, ContainerInfo: make([]*ContainerInfo, 0)}

			for _, c := range svcTmpl.Containers {
				sInfo.ContainerInfo = append(sInfo.ContainerInfo, &ContainerInfo{Value: c.Name, Label: c.Name})
			}

			pInfo.ServiceInfo = append(pInfo.ServiceInfo, sInfo)
		}
		resp = append(resp, pInfo)
	}
	return resp, nil
}

func GetCustomMatchRules(productName string, log *zap.SugaredLogger) ([]*ImageParseData, error) {
	productInfo, err := templaterepo.NewProductColl().Find(productName)
	if err != nil {
		log.Errorf("query product:%s fail, err:%s", productName, err.Error())
		return nil, fmt.Errorf("failed to find product %s", productName)
	}

	rules := productInfo.ImageSearchingRules
	if len(rules) == 0 {
		rules = commonservice.GetPresetRules()
	}

	ret := make([]*ImageParseData, 0, len(rules))
	for _, singleData := range rules {
		ret = append(ret, &ImageParseData{
			Repo:     singleData.Repo,
			Image:    singleData.Image,
			Tag:      singleData.Tag,
			InUse:    singleData.InUse,
			PresetId: singleData.PresetId,
		})
	}
	return ret, nil
}

func UpdateCustomMatchRules(productName string, userName string, matchRules []*ImageParseData) error {
	productInfo, err := templaterepo.NewProductColl().Find(productName)
	if err != nil {
		log.Errorf("query product:%s fail, err:%s", productName, err.Error())
		return fmt.Errorf("failed to find product %s", productName)
	}

	if len(matchRules) == 0 {
		return errors.New("match rules can't be empty")
	}
	haveInUse := false
	for _, rule := range matchRules {
		if rule.InUse {
			haveInUse = true
			break
		}
	}
	if !haveInUse {
		return errors.New("no rule is selected to be used")
	}

	imageRulesToSave := make([]*template.ImageSearchingRule, 0)
	for _, singleData := range matchRules {
		if singleData.Repo == "" && singleData.Image == "" && singleData.Tag == "" {
			continue
		}
		imageRulesToSave = append(imageRulesToSave, &template.ImageSearchingRule{
			Repo:     singleData.Repo,
			Image:    singleData.Image,
			Tag:      singleData.Tag,
			InUse:    singleData.InUse,
			PresetId: singleData.PresetId,
		})
	}

	productInfo.ImageSearchingRules = imageRulesToSave
	productInfo.UpdateBy = userName

	services, err := commonrepo.NewServiceColl().ListMaxRevisionsByProduct(productName)
	if err != nil {
		return err
	}
	err = reParseServices(userName, services, imageRulesToSave)
	if err != nil {
		return err
	}

	err = templaterepo.NewProductColl().Update(productName, productInfo)
	if err != nil {
		log.Errorf("failed to update product:%s, err:%s", productName, err.Error())
		return fmt.Errorf("failed to store match rules")
	}

	return nil
}

// reparse values.yaml for each service
func reParseServices(userName string, serviceList []*commonmodels.Service, matchRules []*template.ImageSearchingRule) error {
	updatedServiceTmpls := make([]*commonmodels.Service, 0)

	var err error
	for _, serviceTmpl := range serviceList {
		if serviceTmpl.Type != setting.HelmDeployType || serviceTmpl.HelmChart == nil {
			continue
		}
		valuesYaml := serviceTmpl.HelmChart.ValuesYaml

		valuesMap := make(map[string]interface{})
		err = yaml.Unmarshal([]byte(valuesYaml), &valuesMap)
		if err != nil {
			err = errors.Wrapf(err, "failed to unmarshal values.yamf for service %s", serviceTmpl.ServiceName)
			break
		}

		serviceTmpl.Containers, err = commonservice.ParseImagesByRules(valuesMap, matchRules)
		if err != nil {
			break
		}

		if len(serviceTmpl.Containers) == 0 {
			log.Warnf("service:%s containers is empty after parse, valuesYaml %s", serviceTmpl.ServiceName, valuesYaml)
		}

		serviceTmpl.CreateBy = userName
		serviceTemplate := fmt.Sprintf(setting.ServiceTemplateCounterName, serviceTmpl.ServiceName, serviceTmpl.ProductName)
		rev, errRevision := commonrepo.NewCounterColl().GetNextSeq(serviceTemplate)
		if errRevision != nil {
			err = fmt.Errorf("get next helm service revision error: %v", errRevision)
			break
		}
		serviceTmpl.Revision = rev
		if err = commonrepo.NewServiceColl().Delete(serviceTmpl.ServiceName, setting.HelmDeployType, serviceTmpl.ProductName, setting.ProductStatusDeleting, serviceTmpl.Revision); err != nil {
			log.Errorf("helmService.update delete %s error: %v", serviceTmpl.ServiceName, err)
			break
		}

		if err = commonrepo.NewServiceColl().Create(serviceTmpl); err != nil {
			log.Errorf("helmService.update serviceName:%s error:%v", serviceTmpl.ServiceName, err)
			err = e.ErrUpdateTemplate.AddDesc(err.Error())
			break
		}

		updatedServiceTmpls = append(updatedServiceTmpls, serviceTmpl)
	}

	// roll back all template services if error occurs
	if err != nil {
		for _, serviceTmpl := range updatedServiceTmpls {
			if err = commonrepo.NewServiceColl().Delete(serviceTmpl.ServiceName, setting.HelmDeployType, serviceTmpl.ProductName, "", serviceTmpl.Revision); err != nil {
				log.Errorf("helmService.update delete %s error: %v", serviceTmpl.ServiceName, err)
				continue
			}
		}
		return err
	}

	return nil
}
