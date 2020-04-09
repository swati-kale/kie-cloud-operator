package defaults

//go:generate go run -mod=vendor .packr/packr.go

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"text/template"

	"github.com/RHsyseng/operator-utils/pkg/logs"
	"github.com/RHsyseng/operator-utils/pkg/utils/kubernetes"
	"github.com/ghodss/yaml"
	"github.com/gobuffalo/packr/v2"
	"github.com/imdario/mergo"
	api "github.com/kiegroup/kie-cloud-operator/pkg/apis/app/v2"
	"github.com/kiegroup/kie-cloud-operator/pkg/controller/kieapp/constants"
	"github.com/kiegroup/kie-cloud-operator/pkg/controller/kieapp/shared"
	"github.com/kiegroup/kie-cloud-operator/version"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
)

var log = logs.GetLogger("kieapp.defaults")

// GetEnvironment returns an Environment from merging the common config and the config
// related to the environment set in the KieApp definition
func GetEnvironment(cr *api.KieApp, service kubernetes.PlatformService) (api.Environment, error) {
	minor, micro, err := checkProductUpgrade(cr)
	if err != nil {
		return api.Environment{}, err
	}
	// handle upgrade logic from here
	cMajor, _, _ := MajorMinorMicro(GetVersion(cr))
	lMajor, _, _ := MajorMinorMicro(constants.CurrentVersion)
	minorVersion := GetMinorImageVersion(GetVersion(cr))
	latestMinorVersion := GetMinorImageVersion(constants.CurrentVersion)
	if (micro && minorVersion == latestMinorVersion) ||
		(minor && minorVersion != latestMinorVersion && cMajor == lMajor) {
		if err := getConfigVersionDiffs(GetVersion(cr), constants.CurrentVersion, service); err != nil {
			return api.Environment{}, err
		}
		// reset current annotations and update CR use to latest product version
		cr.SetAnnotations(map[string]string{})
		SetVersion(cr, constants.CurrentVersion)
	}
	envTemplate, err := getEnvTemplate(cr)
	if err != nil {
		return api.Environment{}, err
	}

	var common api.Environment
	yamlBytes, err := loadYaml(service, "common.yaml", GetVersion(cr), cr.Namespace, envTemplate)
	if err != nil {
		return api.Environment{}, err
	}
	err = yaml.Unmarshal(yamlBytes, &common)
	if err != nil {
		return api.Environment{}, err
	}
	var env api.Environment
	yamlBytes, err = loadYaml(service, fmt.Sprintf("envs/%s.yaml", cr.Spec.Environment), GetVersion(cr), cr.Namespace, envTemplate)
	if err != nil {
		return api.Environment{}, err
	}
	err = yaml.Unmarshal(yamlBytes, &env)
	if err != nil {
		return api.Environment{}, err
	}
	if cr.Spec.Objects.SmartRouter == nil {
		env.SmartRouter.Omit = true
	}

	mergedEnv, err := merge(common, env)
	if err != nil {
		return api.Environment{}, err
	}
	if mergedEnv.SmartRouter.Omit {
		// remove router env vars from kieserver DCs
		for _, server := range mergedEnv.Servers {
			for _, dc := range server.DeploymentConfigs {
				newSlice := []corev1.EnvVar{}
				for _, envvar := range dc.Spec.Template.Spec.Containers[0].Env {
					if envvar.Name != "KIE_SERVER_ROUTER_SERVICE" && envvar.Name != "KIE_SERVER_ROUTER_PORT" && envvar.Name != "KIE_SERVER_ROUTER_PROTOCOL" {
						newSlice = append(newSlice, envvar)
					}
				}
				dc.Spec.Template.Spec.Containers[0].Env = newSlice
			}
		}
	}

	mergedEnv, err = mergeDB(service, cr, mergedEnv, envTemplate)
	if err != nil {
		return api.Environment{}, err
	}
	mergedEnv, err = mergeJms(service, cr, mergedEnv, envTemplate)
	if err != nil {
		return api.Environment{}, err
	}
	mergedEnv, err = mergeProcessMigration(service, cr, mergedEnv, envTemplate)
	if err != nil {
		return api.Environment{}, err
	}
	mergedEnv, err = mergeDBDeployment(service, cr, mergedEnv, envTemplate)
	if err != nil {
		return api.Environment{}, err
	}
	return mergedEnv, nil
}

func mergeDB(service kubernetes.PlatformService, cr *api.KieApp, env api.Environment, envTemplate api.EnvTemplate) (api.Environment, error) {
	dbEnvs := make(map[api.DatabaseType]api.Environment)
	for i := range env.Servers {
		kieServerSet := envTemplate.Servers[i]
		if kieServerSet.Database.Type == "" {
			continue
		}
		dbType := kieServerSet.Database.Type
		if err := loadDBYamls(service, cr, envTemplate, "dbs/servers/%s.yaml", dbType, dbEnvs); err != nil {
			return api.Environment{}, err
		}
		dbServer, found := findCustomObjectByName(env.Servers[i], dbEnvs[dbType].Servers)
		if found {
			env.Servers[i] = mergeCustomObject(env.Servers[i], dbServer)
		}
	}
	return env, nil
}

func mergeJms(service kubernetes.PlatformService, cr *api.KieApp, env api.Environment, envTemplate api.EnvTemplate) (api.Environment, error) {
	var jmsEnv api.Environment
	for i := range env.Servers {
		kieServerSet := envTemplate.Servers[i]

		isJmsEnabled := kieServerSet.Jms.EnableIntegration

		if isJmsEnabled {
			yamlBytes, err := loadYaml(service, fmt.Sprintf("jms/activemq-jms-config.yaml"), GetVersion(cr), cr.Namespace, envTemplate)
			if err != nil {
				return api.Environment{}, err
			}
			err = yaml.Unmarshal(yamlBytes, &jmsEnv)
			if err != nil {
				return api.Environment{}, err
			}
		}

		jmsServer, found := findCustomObjectByName(env.Servers[i], jmsEnv.Servers)
		if found {
			env.Servers[i] = mergeCustomObject(env.Servers[i], jmsServer)
		}
	}
	return env, nil
}

func findCustomObjectByName(template api.CustomObject, objects []api.CustomObject) (api.CustomObject, bool) {
	for i := range objects {
		if len(objects[i].DeploymentConfigs) == 0 || len(template.DeploymentConfigs) == 0 {
			return api.CustomObject{}, false
		}
		if objects[i].DeploymentConfigs[0].ObjectMeta.Name == template.DeploymentConfigs[0].ObjectMeta.Name {
			return objects[i], true
		}
	}
	return api.CustomObject{}, false
}

func getEnvTemplate(cr *api.KieApp) (envTemplate api.EnvTemplate, err error) {
	setDefaults(cr)
	serversConfig, err := getServersConfig(cr)
	if err != nil {
		return envTemplate, err
	}
	envTemplate = api.EnvTemplate{
		Console:     getConsoleTemplate(cr),
		Servers:     serversConfig,
		SmartRouter: getSmartRouterTemplate(cr),
		Constants:   *getTemplateConstants(cr),
	}

	// !!! add checks to only add PIM for only version 7.8.0 and higher ...
	// !!! should also only add 7.8.0 images or higher to csv
	processMigrationConfig, err := getProcessMigrationTemplate(cr, serversConfig)
	if err != nil {
		return envTemplate, err
	}
	envTemplate.ProcessMigration = *processMigrationConfig
	//

	envTemplate.Databases = getDatabaseDeploymentTemplate(cr, serversConfig, processMigrationConfig)
	resolvedSpec, err := ResolveSpec(cr)
	if err != nil {
		return envTemplate, err
	}
	envTemplate.CommonConfig = &resolvedSpec.CommonConfig
	if cr.Spec.Auth != nil {
		if err := configureAuth(cr, &envTemplate); err != nil {
			log.Error("unable to setup authentication: ", err)
			return envTemplate, err
		}
	}

	return envTemplate, nil
}

func getTemplateConstants(cr *api.KieApp) *api.TemplateConstants {
	c := constants.TemplateConstants.DeepCopy()
	c.Major, c.Minor, c.Micro = MajorMinorMicro(GetVersion(cr))
	if envConstants, found := constants.EnvironmentConstants[cr.Spec.Environment]; found {
		c.Product = envConstants.App.Product
		c.MavenRepo = envConstants.App.MavenRepo
	}
	if versionConstants, found := constants.VersionConstants[GetVersion(cr)]; found {
		c.BrokerImage = versionConstants.BrokerImage
		c.BrokerImageTag = versionConstants.BrokerImageTag
		c.DatagridImage = versionConstants.DatagridImage
		c.DatagridImageTag = versionConstants.DatagridImageTag

		c.OseCliImageURL = versionConstants.OseCliImageURL
		c.MySQLImageURL = versionConstants.MySQLImageURL
		c.PostgreSQLImageURL = versionConstants.PostgreSQLImageURL
		c.DatagridImageURL = versionConstants.DatagridImageURL
		c.BrokerImageURL = versionConstants.BrokerImageURL
	}
	if val, exists := os.LookupEnv(constants.OseCliVar + GetVersion(cr)); exists && !cr.Spec.UseImageTags {
		c.OseCliImageURL = val
	}
	if val, exists := os.LookupEnv(constants.MySQLVar + GetVersion(cr)); exists && !cr.Spec.UseImageTags {
		c.MySQLImageURL = val
	}
	if val, exists := os.LookupEnv(constants.PostgreSQLVar + GetVersion(cr)); exists && !cr.Spec.UseImageTags {
		c.PostgreSQLImageURL = val
	}
	if val, exists := os.LookupEnv(constants.DatagridVar + GetVersion(cr)); exists && !cr.Spec.UseImageTags {
		c.DatagridImageURL = val
	}
	if val, exists := os.LookupEnv(constants.BrokerVar + GetVersion(cr)); exists && !cr.Spec.UseImageTags {
		c.BrokerImageURL = val
	}
	return c
}

func getConsoleTemplate(cr *api.KieApp) api.ConsoleTemplate {
	// Set replicas
	consoleReplicas := cr.Status.Generated.Objects.Console.Replicas
	if cr.Spec.Objects.Console.Replicas != nil {
		consoleReplicas = cr.Spec.Objects.Console.Replicas
	}
	template := api.ConsoleTemplate{}
	envConstants, hasEnv := constants.EnvironmentConstants[cr.Spec.Environment]
	if !hasEnv {
		return template
	}
	replicas, denyScale := setReplicas(consoleReplicas, envConstants.Replica.Console, hasEnv)
	if denyScale && cr.Spec.Objects.Console.Replicas != nil {
		cr.Spec.Objects.Console.Replicas = nil
	}
	if cr.Spec.Objects.Console.Replicas == nil {
		cr.Status.Generated.Objects.Console.Replicas = Pint32(replicas)
	}
	resolvedSpec, err := ResolveSpec(cr)
	if err != nil {
		log.Error("Could not Resolve spec - ", err)
		return template
	}
	template.Replicas = *resolvedSpec.Objects.Console.Replicas
	template.Name = envConstants.App.Prefix
	template.ImageURL = envConstants.App.Product + "-" + envConstants.App.ImageName + constants.RhelVersion + ":" + GetVersion(cr)
	template.KeystoreSecret = resolvedSpec.Objects.Console.KeystoreSecret
	if template.KeystoreSecret == "" {
		template.KeystoreSecret = fmt.Sprintf(constants.KeystoreSecret, strings.Join([]string{resolvedSpec.CommonConfig.ApplicationName, "businesscentral"}, "-"))
	}
	template.StorageClassName = resolvedSpec.Objects.Console.StorageClassName
	if val, exists := os.LookupEnv(envConstants.App.ImageVar + GetVersion(cr)); exists && !resolvedSpec.UseImageTags {
		template.ImageURL = val
		template.OmitImageStream = true
	}
	template.Image, template.ImageTag, _ = GetImage(template.ImageURL)
	if resolvedSpec.Objects.Console.Image != "" {
		template.Image = resolvedSpec.Objects.Console.Image
		template.ImageURL = template.Image + ":" + template.ImageTag
		template.OmitImageStream = false
	}
	if resolvedSpec.Objects.Console.ImageTag != "" {
		template.ImageTag = resolvedSpec.Objects.Console.ImageTag
		template.ImageURL = template.Image + ":" + template.ImageTag
		template.OmitImageStream = false
	}
	if resolvedSpec.Objects.Console.GitHooks != nil {
		template.GitHooks = *resolvedSpec.Objects.Console.GitHooks.DeepCopy()
		if template.GitHooks.MountPath == "" {
			template.GitHooks.MountPath = constants.GitHooksDefaultDir
		}
	}
	// JVM configuration
	if resolvedSpec.Objects.Console.Jvm != nil {
		template.Jvm = *resolvedSpec.Objects.Console.Jvm.DeepCopy()
	}
	return template
}

func getSmartRouterTemplate(cr *api.KieApp) api.SmartRouterTemplate {
	template := api.SmartRouterTemplate{}
	if cr.Spec.Objects.SmartRouter != nil {
		envConstants, hasEnv := constants.EnvironmentConstants[cr.Spec.Environment]
		if !hasEnv {
			return template
		}
		// Set replicas
		cr.Status.Generated.Objects.SmartRouter = nil
		if cr.Spec.Objects.SmartRouter.Replicas == nil {
			cr.Status.Generated.Objects.SmartRouter = &api.SmartRouterObject{KieAppObject: api.KieAppObject{Replicas: &envConstants.Replica.SmartRouter.Replicas}}
		}
		resolvedSpec, err := ResolveSpec(cr)
		if err != nil {
			log.Error("Could not Resolve spec - ", err)
			return template
		}
		template.Replicas = *resolvedSpec.Objects.SmartRouter.Replicas
		if cr.Spec.Objects.SmartRouter.KeystoreSecret == "" {
			template.KeystoreSecret = fmt.Sprintf(constants.KeystoreSecret, strings.Join([]string{resolvedSpec.CommonConfig.ApplicationName, "smartrouter"}, "-"))
		} else {
			template.KeystoreSecret = cr.Spec.Objects.SmartRouter.KeystoreSecret
		}
		if cr.Spec.Objects.SmartRouter.Protocol == "" {
			template.Protocol = constants.SmartRouterProtocol
		} else {
			template.Protocol = cr.Spec.Objects.SmartRouter.Protocol
		}
		template.UseExternalRoute = cr.Spec.Objects.SmartRouter.UseExternalRoute
		template.StorageClassName = cr.Spec.Objects.SmartRouter.StorageClassName
		template.ImageURL = constants.RhpamPrefix + "-smartrouter" + constants.RhelVersion + ":" + GetVersion(cr)
		if val, exists := os.LookupEnv(constants.PamSmartRouterVar + GetVersion(cr)); exists && !cr.Spec.UseImageTags {
			template.ImageURL = val
			template.OmitImageStream = true
		}
		template.Image, template.ImageTag, _ = GetImage(template.ImageURL)

		if cr.Spec.Objects.SmartRouter.Image != "" {
			template.Image = cr.Spec.Objects.SmartRouter.Image
			template.ImageURL = template.Image + ":" + template.ImageTag
			template.OmitImageStream = false
		}
		if cr.Spec.Objects.SmartRouter.ImageTag != "" {
			template.ImageTag = cr.Spec.Objects.SmartRouter.ImageTag
			template.ImageURL = template.Image + ":" + template.ImageTag
			template.OmitImageStream = false
		}
	}
	return template
}

// GetImage ...
func GetImage(imageURL string) (image, imageTag, imageContext string) {
	urlParts := strings.Split(imageURL, "/")
	if len(urlParts) > 1 {
		imageContext = urlParts[len(urlParts)-2]
	}
	imageAndTag := urlParts[len(urlParts)-1]
	imageParts := strings.Split(imageAndTag, ":")
	image = imageParts[0]
	if len(imageParts) > 1 {
		imageTag = imageParts[len(imageParts)-1]
	}
	return image, imageTag, imageContext
}

func setReplicas(objectReplicas *int32, replicaConstant api.Replicas, hasEnv bool) (replicas int32, denyScale bool) {
	if objectReplicas != nil {
		if hasEnv && replicaConstant.DenyScale && *objectReplicas != replicaConstant.Replicas {
			log.Warnf("scaling not allowed for this environment, setting to default of %d", replicaConstant.Replicas)
			return replicaConstant.Replicas, true
		}
		return *objectReplicas, false
	}
	if hasEnv {
		return replicaConstant.Replicas, false
	}
	log.Warnf("no replicas settings for this environment, defaulting to %d", replicas)
	return int32(1), denyScale
}

// serverSortBlanks moves blank names to the end
func serverSortBlanks(serverSets []api.KieServerSet) []api.KieServerSet {
	var newSets []api.KieServerSet
	// servers with existing names should be placed in front
	for index := range serverSets {
		if serverSets[index].Name != "" {
			newSets = append(newSets, serverSets[index])
		}
	}
	// servers without names should be at the end
	for index := range serverSets {
		if serverSets[index].Name == "" {
			newSets = append(newSets, serverSets[index])
		}
	}
	if len(newSets) != len(serverSets) {
		log.Error("slice lengths aren't equal, returning server sets w/o blank names sorted")
		return serverSets
	}
	return newSets
}

// Returns the templates to use depending on whether the spec was defined with a common configuration
// or a specific one.
func getServersConfig(cr *api.KieApp) ([]api.ServerTemplate, error) {
	var servers []api.ServerTemplate
	serverReplicas := Pint32(1)
	envConstants, hasEnv := constants.EnvironmentConstants[cr.Spec.Environment]
	if hasEnv {
		serverReplicas = &envConstants.Replica.Server.Replicas
	}
	product := GetProduct(cr.Spec.Environment)
	usedNames := map[string]bool{}
	unsetNames := 0
	cr.Status.Generated.Objects.Servers = serverSortBlanks(cr.Status.Generated.Objects.Servers)
	serverSlice := cr.Status.Generated.Objects.Servers
	if len(cr.Spec.Objects.Servers) != 0 {
		cr.Status.Generated.Objects.Servers = nil
		cr.Spec.Objects.Servers = serverSortBlanks(cr.Spec.Objects.Servers)
		serverSlice = cr.Spec.Objects.Servers
	}
	appName := cr.Status.Generated.CommonConfig.ApplicationName
	if len(cr.Spec.CommonConfig.ApplicationName) != 0 {
		appName = cr.Spec.CommonConfig.ApplicationName
	}
	for index := range serverSlice {
		serverSet := &serverSlice[index]
		if serverSet.Deployments == nil {
			serverSet.Deployments = Pint(constants.DefaultKieDeployments)
		}
		if serverSet.Name == "" {
			for i := 0; i < len(serverSlice); i++ {
				serverSetName := getKieSetName(appName, serverSet.Name, unsetNames)
				if !usedNames[serverSetName] {
					serverSet.Name = serverSetName
					break
				}
				unsetNames++
			}
		}
		for i := 0; i < *serverSet.Deployments; i++ {
			name := getKieDeploymentName(appName, serverSet.Name, unsetNames, i)
			if usedNames[name] {
				return []api.ServerTemplate{}, fmt.Errorf("duplicate kieserver name %s", name)
			}
			usedNames[name] = true
			template := api.ServerTemplate{
				KieName:          name,
				KieServerID:      name,
				Build:            getBuildConfig(product, cr, serverSet),
				KeystoreSecret:   serverSet.KeystoreSecret,
				StorageClassName: serverSet.StorageClassName,
			}
			if serverSet.ID != "" {
				template.KieServerID = serverSet.ID
			}
			if serverSet.Build != nil {
				if *serverSet.Deployments > 1 {
					return []api.ServerTemplate{}, fmt.Errorf("Cannot request %v deployments for a build", *serverSet.Deployments)
				}
				template.From = corev1.ObjectReference{
					Kind:      "ImageStreamTag",
					Name:      fmt.Sprintf("%s-kieserver:latest", appName),
					Namespace: "",
				}
			} else {
				template.From, template.OmitImageStream, template.ImageURL = getDefaultKieServerImage(product, cr, serverSet)
			}

			// Set replicas
			if serverSet.Replicas == nil {
				serverSet.Replicas = serverReplicas
			}
			template.Replicas = *serverSet.Replicas

			// if, SmartRouter object is nil, ignore it
			// get smart router protocol configuration
			if cr.Spec.Objects.SmartRouter != nil {
				if cr.Spec.Objects.SmartRouter.Protocol == "" {
					template.SmartRouter.Protocol = constants.SmartRouterProtocol
				} else {
					template.SmartRouter.Protocol = cr.Spec.Objects.SmartRouter.Protocol
				}
			}

			dbConfig, err := getDatabaseConfig(cr.Spec.Environment, serverSet.Database)
			if err != nil {
				return servers, err
			}
			if dbConfig != nil {
				template.Database = *dbConfig
			}

			jmsConfig, err := getJmsConfig(cr.Spec.Environment, serverSet.Jms)
			if err != nil {
				return servers, err
			}
			if jmsConfig != nil {
				template.Jms = *jmsConfig
			}

			instanceTemplate := template.DeepCopy()
			if instanceTemplate.KeystoreSecret == "" {
				instanceTemplate.KeystoreSecret = fmt.Sprintf(constants.KeystoreSecret, instanceTemplate.KieName)
			}

			// JVM configuration
			if serverSet.Jvm != nil {
				instanceTemplate.Jvm = *serverSet.Jvm.DeepCopy()
			}

			servers = append(servers, *instanceTemplate)
		}
	}
	return servers, nil
}

// GetServerSet retrieves to correct ServerSet for processing and the DeploymentName
func GetServerSet(cr *api.KieApp, requestedIndex int) (serverSet api.KieServerSet, kieName string) {
	count := 0
	unnamedSets := 0
	resolvedSpec, err := ResolveSpec(cr)
	if err != nil {
		return
	}
	for _, thisServerSet := range resolvedSpec.Objects.Servers {
		for relativeIndex := 0; relativeIndex < *thisServerSet.Deployments; relativeIndex++ {
			if count == requestedIndex {
				serverSet = thisServerSet
				kieName = getKieDeploymentName(resolvedSpec.CommonConfig.ApplicationName, serverSet.Name, unnamedSets, relativeIndex)
				return
			}
			count++
		}
		if thisServerSet.Name == "" {
			unnamedSets++
		}
	}
	return
}

// ConsolidateObjects construct all CustomObjects prior to creation
func ConsolidateObjects(env api.Environment, cr *api.KieApp) api.Environment {
	resolvedSpec, err := ResolveSpec(cr)
	if err != nil {
		log.Error(err)
		return api.Environment{}
	}
	env.Console = ConstructObject(env.Console, resolvedSpec.Objects.Console.KieAppObject)
	if resolvedSpec.Objects.SmartRouter != nil {
		env.SmartRouter = ConstructObject(env.SmartRouter, resolvedSpec.Objects.SmartRouter.KieAppObject)
	}
	for index := range env.Servers {
		serverSet, _ := GetServerSet(cr, index)
		env.Servers[index] = ConstructObject(env.Servers[index], serverSet.KieAppObject)
	}
	return env
}

// ConstructObject returns an object after merging the environment object and the one defined in the CR
func ConstructObject(object api.CustomObject, appObject api.KieAppObject) api.CustomObject {
	for dcIndex, dc := range object.DeploymentConfigs {
		for containerIndex, c := range dc.Spec.Template.Spec.Containers {
			c.Env = shared.EnvOverride(c.Env, appObject.Env)
			if appObject.Resources != nil {
				err := mergo.Merge(&c.Resources, *appObject.Resources, mergo.WithOverride)
				if err != nil {
					log.Error("Error merging interfaces. ", err)
				}
			}
			dc.Spec.Template.Spec.Containers[containerIndex] = c
		}
		object.DeploymentConfigs[dcIndex] = dc
	}
	return object
}

// getKieSetName aids in server indexing, depending on number of deployments and sets
func getKieSetName(applicationName string, setName string, arrayIdx int) string {
	return getKieDeploymentName(applicationName, setName, arrayIdx, 0)
}

func getKieDeploymentName(applicationName string, setName string, arrayIdx, deploymentsIdx int) string {
	name := setName
	if name == "" {
		name = fmt.Sprintf("%v-kieserver%v", applicationName, getKieSetIndex(arrayIdx, deploymentsIdx))
	} else {
		name = fmt.Sprintf("%v%v", setName, getKieSetIndex(0, deploymentsIdx))
	}
	return name
}

func getKieSetIndex(arrayIdx, deploymentsIdx int) string {
	var name bytes.Buffer
	if arrayIdx > 0 {
		name.WriteString(fmt.Sprintf("%d", arrayIdx+1))
	}
	if deploymentsIdx > 0 {
		name.WriteString(fmt.Sprintf("-%d", deploymentsIdx+1))
	}
	return name.String()
}

func getBuildConfig(product string, cr *api.KieApp, serverSet *api.KieServerSet) api.BuildTemplate {
	if serverSet.Build == nil {
		return api.BuildTemplate{}
	}
	buildTemplate := api.BuildTemplate{}

	if serverSet.Build.ExtensionImageStreamTag != "" {
		if serverSet.Build.ExtensionImageStreamTagNamespace == "" {
			serverSet.Build.ExtensionImageStreamTagNamespace = constants.ImageStreamNamespace
			log.Debugf("Extension Image Stream Tag set but no namespace set, defaulting to %s", serverSet.Build.ExtensionImageStreamTagNamespace)
		}
		if serverSet.Build.ExtensionImageInstallDir == "" {
			serverSet.Build.ExtensionImageInstallDir = constants.DefaultExtensionImageInstallDir
		} else {
			log.Debugf("Extension Image Install Dir set to %s, be cautions when updating this parameter.", serverSet.Build.ExtensionImageInstallDir)
		}
		// JDBC extension image build template
		buildTemplate = api.BuildTemplate{
			ExtensionImageStreamTag:          serverSet.Build.ExtensionImageStreamTag,
			ExtensionImageStreamTagNamespace: serverSet.Build.ExtensionImageStreamTagNamespace,
			ExtensionImageInstallDir:         serverSet.Build.ExtensionImageInstallDir,
		}

	} else {
		// build app from source template
		buildTemplate = api.BuildTemplate{
			GitSource:                    serverSet.Build.GitSource,
			GitHubWebhookSecret:          getWebhookSecret(api.GitHubWebhook, serverSet.Build.Webhooks),
			GenericWebhookSecret:         getWebhookSecret(api.GenericWebhook, serverSet.Build.Webhooks),
			KieServerContainerDeployment: serverSet.Build.KieServerContainerDeployment,
			MavenMirrorURL:               serverSet.Build.MavenMirrorURL,
			ArtifactDir:                  serverSet.Build.ArtifactDir,
		}
	}
	buildTemplate.From, _, _ = getDefaultKieServerImage(product, cr, serverSet)
	if serverSet.Build.From != nil {
		buildTemplate.From = *serverSet.Build.From
	}

	return buildTemplate
}

func getDefaultKieServerImage(product string, cr *api.KieApp, serverSet *api.KieServerSet) (from corev1.ObjectReference, omitImageTrigger bool, imageURL string) {
	if serverSet.From != nil {
		return *serverSet.From, omitImageTrigger, imageURL
	}
	envVar := constants.PamKieImageVar + GetVersion(cr)
	if product == constants.RhdmPrefix {
		envVar = constants.DmKieImageVar + GetVersion(cr)
	}

	imageURL = product + "-kieserver" + constants.RhelVersion + ":" + GetVersion(cr)
	if val, exists := os.LookupEnv(envVar); exists && !cr.Spec.UseImageTags {
		imageURL = val
		omitImageTrigger = true
	}
	image, imageTag, _ := GetImage(imageURL)

	if serverSet.Image != "" {
		image = serverSet.Image
		imageURL = image + ":" + imageTag
		omitImageTrigger = false
	}
	if serverSet.ImageTag != "" {
		imageTag = serverSet.ImageTag
		imageURL = image + ":" + imageTag
		omitImageTrigger = false
	}

	return corev1.ObjectReference{
		Kind:      "ImageStreamTag",
		Name:      image + ":" + imageTag,
		Namespace: constants.ImageStreamNamespace,
	}, omitImageTrigger, imageURL
}

func getDatabaseConfig(environment api.EnvironmentType, database *api.DatabaseObject) (*api.DatabaseObject, error) {
	envConstants := constants.EnvironmentConstants[environment]
	if envConstants == nil {
		return nil, nil
	}
	defaultDB := envConstants.Database
	if database == nil {
		return defaultDB, nil
	}

	if database.Type == api.DatabaseExternal && database.ExternalConfig == nil {
		return nil, fmt.Errorf("external database configuration is mandatory for external database type")
	}

	if database.Size == "" && defaultDB != nil {
		resultDB := *database.DeepCopy()
		resultDB.Size = defaultDB.Size
		return &resultDB, nil
	}
	return database, nil
}

func getJmsConfig(environment api.EnvironmentType, jms *api.KieAppJmsObject) (*api.KieAppJmsObject, error) {

	envConstants := constants.EnvironmentConstants[environment]
	if envConstants == nil || jms == nil || !jms.EnableIntegration {
		return nil, nil
	}

	if jms.AMQSecretName != "" && jms.AMQKeystoreName != "" && jms.AMQKeystorePassword != "" &&
		jms.AMQTruststoreName != "" && jms.AMQTruststorePassword != "" {
		jms.AMQEnableSSL = true
	}

	t := true
	if jms.Executor == nil {
		jms.Executor = &t
	}
	if jms.AuditTransacted == nil {
		jms.AuditTransacted = &t
	}

	// if enabled, prepare the default values
	defaultJms := &api.KieAppJmsObject{
		QueueExecutor: "queue/KIE.SERVER.EXECUTOR",
		QueueRequest:  "queue/KIE.SERVER.REQUEST",
		QueueResponse: "queue/KIE.SERVER.RESPONSE",
		QueueSignal:   "queue/KIE.SERVER.SIGNAL",
		QueueAudit:    "queue/KIE.SERVER.AUDIT",
		Username:      "user" + string(shared.GeneratePassword(4)),
		Password:      string(shared.GeneratePassword(8)),
	}

	queuesList := []string{
		getDefaultQueue(*jms.Executor, defaultJms.QueueExecutor, jms.QueueExecutor),
		getDefaultQueue(true, defaultJms.QueueRequest, jms.QueueRequest),
		getDefaultQueue(true, defaultJms.QueueResponse, jms.QueueResponse),
		getDefaultQueue(jms.EnableSignal, defaultJms.QueueSignal, jms.QueueSignal),
		getDefaultQueue(jms.EnableAudit, defaultJms.QueueAudit, jms.QueueAudit),
	}

	// clean empty values
	for i, queue := range queuesList {
		if queue == "" {
			queuesList = append(queuesList[:i], queuesList[i+1:]...)
			break
		}
	}
	defaultJms.AMQQueues = strings.Join(queuesList[:], ", ")

	// merge the defaultJms into jms, preserving the values set by cr
	mergo.Merge(jms, defaultJms)

	return jms, nil
}

func getDefaultQueue(append bool, defaultJmsQueue string, jmsQueue string) string {
	if append {
		if jmsQueue == "" {
			return defaultJmsQueue
		}
		return jmsQueue
	}
	return ""
}

func setPasswords(cr *api.KieApp, isTrialEnv bool) {
	passwords := []*string{
		&cr.Status.Generated.CommonConfig.KeyStorePassword,
		&cr.Status.Generated.CommonConfig.AdminPassword,
		&cr.Status.Generated.CommonConfig.DBPassword,
		&cr.Status.Generated.CommonConfig.AMQPassword,
		&cr.Status.Generated.CommonConfig.AMQClusterPassword,
	}
	for i := range passwords {
		if len(*passwords[i]) > 0 {
			continue
		}
		if isTrialEnv {
			*passwords[i] = constants.DefaultPassword
		} else {
			*passwords[i] = string(shared.GeneratePassword(8))
		}
	}
	if len(cr.Spec.CommonConfig.KeyStorePassword) != 0 {
		cr.Status.Generated.CommonConfig.KeyStorePassword = ""
	}
	if len(cr.Spec.CommonConfig.AdminPassword) != 0 {
		cr.Status.Generated.CommonConfig.AdminPassword = ""
	}
	if len(cr.Spec.CommonConfig.DBPassword) != 0 {
		cr.Status.Generated.CommonConfig.DBPassword = ""
	}
	if len(cr.Spec.CommonConfig.AMQPassword) != 0 {
		cr.Status.Generated.CommonConfig.AMQPassword = ""
	}
	if len(cr.Spec.CommonConfig.AMQClusterPassword) != 0 {
		cr.Status.Generated.CommonConfig.AMQClusterPassword = ""
	}
}

func getWebhookSecret(webhookType api.WebhookType, webhooks []api.WebhookSecret) string {
	for _, webhook := range webhooks {
		if webhook.Type == webhookType {
			return webhook.Secret
		}
	}
	return string(shared.GeneratePassword(8))
}

// important to parse template first with this function, before unmarshalling into object
func loadYaml(service kubernetes.PlatformService, filename, productVersion, namespace string, env api.EnvTemplate) ([]byte, error) {
	// prepend specified product version dir to filepath
	filename = strings.Join([]string{productVersion, filename}, "/")
	if _, _, useEmbedded := UseEmbeddedFiles(service); useEmbedded {
		box := packr.New("config", "../../../../config")
		if !box.HasDir(productVersion) {
			return nil, fmt.Errorf("Product version %s configs are not available in this Operator, %s", productVersion, version.Version)
		}
		if box.Has(filename) {
			yamlString, err := box.FindString(filename)
			if err != nil {
				return nil, err
			}
			return parseTemplate(env, yamlString)
		}
		return nil, fmt.Errorf("%s does not exist, '%s' KieApp not deployed", filename, env.ApplicationName)
	}

	cmName, file := convertToConfigMapName(filename)
	configMap := &corev1.ConfigMap{}
	err := service.Get(context.TODO(), types.NamespacedName{Name: cmName, Namespace: namespace}, configMap)
	if err != nil {
		return nil, fmt.Errorf("%s/%s ConfigMap not yet accessible, '%s' KieApp not deployed. Retrying... ", namespace, cmName, env.ApplicationName)
	}
	log.Debugf("Reconciling '%s' KieApp with %s from ConfigMap '%s'", env.ApplicationName, file, cmName)
	return parseTemplate(env, configMap.Data[file])
}

func parseTemplate(env api.EnvTemplate, objYaml string) ([]byte, error) {
	var b bytes.Buffer

	tmpl, err := template.New(env.ApplicationName).Delims("[[", "]]").Parse(objYaml)
	if err != nil {
		log.Error("Error creating new Go template.")
		return []byte{}, err
	}

	// template replacement
	err = tmpl.Execute(&b, env)
	if err != nil {
		log.Error("Error applying Go template.")
		return []byte{}, err
	}

	return b.Bytes(), nil
}

// convertToConfigMapName ...
func convertToConfigMapName(filename string) (configMapName, file string) {
	name := constants.ConfigMapPrefix
	result := strings.Split(filename, "/")
	if len(result) > 1 {
		for i := 0; i < len(result)-1; i++ {
			name = strings.Join([]string{name, result[i]}, "-")
		}
	}
	return name, result[len(result)-1]
}

// getCMListfromBox reads the files under the config folder ...
func getCMListfromBox(box *packr.Box) map[string][]map[string]string {
	cmList := map[string][]map[string]string{}
	for _, filename := range box.List() {
		s, err := box.FindString(filename)
		if err != nil {
			log.Error("Error finding file with packr. ", err)
		}
		cmData := map[string]string{}
		cmName, file := convertToConfigMapName(filename)
		cmData[file] = s
		cmList[cmName] = append(cmList[cmName], cmData)
	}
	return cmList
}

// ConfigMapsFromFile reads the files under the config folder and creates
// configmaps in the given namespace. It sets OwnerRef to operator deployment.
func ConfigMapsFromFile(myDep *appsv1.Deployment, ns string, scheme *runtime.Scheme) (configMaps []corev1.ConfigMap) {
	box := packr.New("config", "../../../../config")
	cmList := getCMListfromBox(box)
	for cmName, dataSlice := range cmList {
		cmData := map[string]string{}
		for _, dataList := range dataSlice {
			for name, data := range dataList {
				cmData[name] = data
			}
		}
		cm := corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      cmName,
				Namespace: ns,
				Annotations: map[string]string{
					api.SchemeGroupVersion.Group: version.Version,
				},
			},
			Data: cmData,
		}

		cm.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("ConfigMap"))
		cm.SetOwnerReferences(myDep.GetOwnerReferences())
		configMaps = append(configMaps, cm)
	}
	return configMaps
}

// UseEmbeddedFiles checks environment variables WATCH_NAMESPACE & OPERATOR_NAME
func UseEmbeddedFiles(service kubernetes.PlatformService) (opName string, depNameSpace string, useEmbedded bool) {
	namespace := os.Getenv(constants.NameSpaceEnv)
	name := os.Getenv(constants.OpNameEnv)
	if service.IsMockService() || namespace == "" || name == "" {
		return name, namespace, true
	}
	return name, namespace, false
}

// Pint returns a pointer to an integer
func Pint(i int) *int {
	return &i
}

// Pint32 returns a pointer to an integer
func Pint32(i int32) *int32 {
	return &i
}

// Pbool returns a pointer to a boolean
func Pbool(b bool) *bool {
	return &b
}

// GetProduct ...
func GetProduct(env api.EnvironmentType) (product string) {
	envConstants := constants.EnvironmentConstants[env]
	if envConstants != nil {
		product = envConstants.App.Product
	}
	return
}

// ResolveSpec returns a combined spec which can be used for product template configuration
func ResolveSpec(cr *api.KieApp) (api.KieAppSpec, error) {
	mergedSpec := cr.Spec.DeepCopy()
	err := mergo.Merge(mergedSpec, cr.Status.Generated)
	return *mergedSpec, err
}

// GetVersion returns set KieApp version
func GetVersion(cr *api.KieApp) (v string) {
	if cr.Spec.Version != "" {
		return cr.Spec.Version
	}
	return cr.Status.Generated.Version
}

// SetVersion sets KieApp version
func SetVersion(cr *api.KieApp, v string) {
	if cr.Spec.Version != "" {
		cr.Spec.Version = v
		return
	}
	cr.Status.Generated.Version = v
}

// setDefaults set default values where not provided
func setDefaults(cr *api.KieApp) {
	if cr.GetAnnotations() == nil {
		cr.SetAnnotations(map[string]string{
			api.SchemeGroupVersion.Group: version.Version,
		})
	}
	cr.Status.Generated.Version = constants.CurrentVersion
	if len(cr.Spec.Version) != 0 {
		cr.Status.Generated.Version = ""
	}
	cr.Status.Generated.CommonConfig.ApplicationName = cr.Name
	if len(cr.Spec.CommonConfig.ApplicationName) != 0 {
		cr.Status.Generated.CommonConfig.ApplicationName = ""
	}
	cr.Status.Generated.CommonConfig.AdminUser = constants.DefaultAdminUser
	if len(cr.Spec.CommonConfig.AdminUser) != 0 {
		cr.Status.Generated.CommonConfig.AdminUser = ""
	}
	cr.Status.Generated.Objects.Servers = []api.KieServerSet{{Deployments: Pint(constants.DefaultKieDeployments)}}
	if len(cr.Spec.Objects.Servers) != 0 {
		cr.Status.Generated.Objects.Servers = nil
	}
	if cr.Spec.Objects.Console.Replicas != nil {
		cr.Status.Generated.Objects.Console.Replicas = nil
	}
	isTrialEnv := strings.HasSuffix(string(cr.Spec.Environment), constants.TrialEnvSuffix)
	setPasswords(cr, isTrialEnv)
}

func getProcessMigrationTemplate(cr *api.KieApp, serversConfig []api.ServerTemplate) (*api.ProcessMigrationTemplate, error) {
	processMigrationTemplate := api.ProcessMigrationTemplate{}
	if deployProcessMigration(cr) {
		resolvedSpec, err := ResolveSpec(cr)
		if err != nil {
			return &processMigrationTemplate, err
		}
		processMigrationTemplate.ImageURL = constants.RhpamPrefix + "-process-migration" + constants.RhelVersion + ":" + GetVersion(cr)
		if val, exists := os.LookupEnv(constants.PamProcessMigrationVar + GetVersion(cr)); exists && !cr.Spec.UseImageTags {
			processMigrationTemplate.ImageURL = val
			processMigrationTemplate.OmitImageStream = true
		}
		processMigrationTemplate.Image, processMigrationTemplate.ImageTag, _ = GetImage(processMigrationTemplate.ImageURL)
		if cr.Spec.Objects.ProcessMigration.Image != "" {
			processMigrationTemplate.Image = cr.Spec.Objects.ProcessMigration.Image
			processMigrationTemplate.ImageURL = processMigrationTemplate.Image + ":" + processMigrationTemplate.ImageTag
			processMigrationTemplate.OmitImageStream = false
		}
		if cr.Spec.Objects.ProcessMigration.ImageTag != "" {
			processMigrationTemplate.ImageTag = cr.Spec.Objects.ProcessMigration.ImageTag
			processMigrationTemplate.ImageURL = processMigrationTemplate.Image + ":" + processMigrationTemplate.ImageTag
			processMigrationTemplate.OmitImageStream = false
		}
		for _, sc := range serversConfig {
			processMigrationTemplate.KieServerClients = append(processMigrationTemplate.KieServerClients, api.KieServerClient{
				Host:     fmt.Sprintf("http://%s:8080/services/rest/server", sc.KieName),
				Username: resolvedSpec.CommonConfig.AdminUser,
				Password: resolvedSpec.CommonConfig.AdminPassword,
			})
		}
		if cr.Spec.Objects.ProcessMigration.Database.Type == "" {
			processMigrationTemplate.Database.Type = constants.DefaultProcessMigrationDatabaseType
		} else if cr.Spec.Objects.ProcessMigration.Database.Type == api.DatabaseExternal &&
			cr.Spec.Objects.ProcessMigration.Database.ExternalConfig == nil {
			return nil, fmt.Errorf("external database configuration is mandatory for external database type of process migration")
		} else {
			processMigrationTemplate.Database = *cr.Spec.Objects.ProcessMigration.Database.DeepCopy()
		}
	}
	return &processMigrationTemplate, nil
}

func mergeProcessMigration(service kubernetes.PlatformService, cr *api.KieApp, env api.Environment, envTemplate api.EnvTemplate) (api.Environment, error) {
	var ProcessMigrationEnv api.Environment
	if deployProcessMigration(cr) {
		yamlBytes, err := loadYaml(service, "pim/process-migration.yaml", GetVersion(cr), cr.Namespace, envTemplate)
		if err != nil {
			return api.Environment{}, err
		}
		err = yaml.Unmarshal(yamlBytes, &ProcessMigrationEnv)
		if err != nil {
			return api.Environment{}, err
		}

		env.ProcessMigration = mergeCustomObject(env.ProcessMigration, ProcessMigrationEnv.ProcessMigration)

		env, err = mergeProcessMigrationDB(service, cr, env, envTemplate)
		if err != nil {
			return api.Environment{}, nil
		}
	} else {
		ProcessMigrationEnv.ProcessMigration.Omit = true
	}

	return env, nil
}

func mergeProcessMigrationDB(service kubernetes.PlatformService, cr *api.KieApp, env api.Environment, envTemplate api.EnvTemplate) (api.Environment, error) {
	if envTemplate.ProcessMigration.Database.Type == api.DatabaseH2 {
		return env, nil
	}
	yamlBytes, err := loadYaml(service, fmt.Sprintf("dbs/pim/%s.yaml", envTemplate.ProcessMigration.Database.Type), GetVersion(cr), cr.Namespace, envTemplate)
	if err != nil {
		return api.Environment{}, err
	}
	var dbEnv api.Environment
	err = yaml.Unmarshal(yamlBytes, &dbEnv)
	if err != nil {
		return api.Environment{}, err
	}
	env.ProcessMigration = mergeCustomObject(env.ProcessMigration, dbEnv.ProcessMigration)

	return env, nil
}

func isRHPAM(cr *api.KieApp) bool {
	switch cr.Spec.Environment {
	case api.RhpamTrial, api.RhpamAuthoring, api.RhpamAuthoringHA, api.RhpamProduction, api.RhpamProductionImmutable:
		return true
	}
	return false
}

func deployProcessMigration(cr *api.KieApp) bool {
	return cr.Spec.Objects.ProcessMigration != nil && isRHPAM(cr)
}

func getDatabaseDeploymentTemplate(cr *api.KieApp, serversConfig []api.ServerTemplate,
	processMigrationTemplate *api.ProcessMigrationTemplate) []api.DatabaseTemplate {
	var databaseDeploymentTemplate []api.DatabaseTemplate
	if serversConfig != nil {
		for _, sc := range serversConfig {
			if isDeployDB(sc.Database.Type) {
				databaseDeploymentTemplate = append(databaseDeploymentTemplate, api.DatabaseTemplate{
					InternalDatabaseObject: sc.Database.InternalDatabaseObject,
					ServerName:             sc.KieName,
					Username:               constants.DefaultKieServerDatabaseUsername,
					DatabaseName:           constants.DefaultKieServerDatabaseName,
				})
			}
		}
	}
	if processMigrationTemplate != nil && isDeployDB(processMigrationTemplate.Database.Type) {
		databaseDeploymentTemplate = append(databaseDeploymentTemplate, api.DatabaseTemplate{
			InternalDatabaseObject: processMigrationTemplate.Database.InternalDatabaseObject,
			ServerName:             cr.Name + "-process-migration",
			Username:               constants.DefaultProcessMigrationDatabaseUsername,
			DatabaseName:           constants.DefaultProcessMigrationDatabaseName,
		})
	}
	return databaseDeploymentTemplate
}

func mergeDBDeployment(service kubernetes.PlatformService, cr *api.KieApp, env api.Environment, envTemplate api.EnvTemplate) (api.Environment, error) {
	env.Databases = make([]api.CustomObject, len(envTemplate.Databases))
	if envTemplate.Databases == nil {
		return env, nil
	}
	dbEnvs := make(map[api.DatabaseType]api.Environment)
	for i, dbTemplate := range envTemplate.Databases {
		if err := loadDBYamls(service, cr, envTemplate, "dbs/%s.yaml", dbTemplate.Type, dbEnvs); err != nil {
			return api.Environment{}, err
		}
		deploymentName := dbTemplate.ServerName + "-" + string(dbTemplate.Type)
		for _, db := range dbEnvs[dbTemplate.Type].Databases {
			if len(db.DeploymentConfigs) == 0 {
				continue
			}
			if deploymentName == db.DeploymentConfigs[0].ObjectMeta.Name {
				env.Databases[i] = mergeCustomObject(env.Databases[i], db)
			}
		}
	}
	return env, nil
}

func loadDBYamls(service kubernetes.PlatformService, cr *api.KieApp, envTemplate api.EnvTemplate,
	dbTemplates string, dbType api.DatabaseType, dbEnvs map[api.DatabaseType]api.Environment) error {
	if _, loadedDB := dbEnvs[dbType]; !loadedDB {
		yamlBytes, err := loadYaml(service, fmt.Sprintf(dbTemplates, dbType), GetVersion(cr), cr.Namespace, envTemplate)
		if err != nil {
			return err
		}
		var dbEnv api.Environment
		err = yaml.Unmarshal(yamlBytes, &dbEnv)
		if err != nil {
			return err
		}
		dbEnvs[dbType] = dbEnv
	}
	return nil
}

func isDeployDB(dbType api.DatabaseType) bool {
	switch dbType {
	case api.DatabaseMySQL, api.DatabasePostgreSQL:
		return true
	}
	return false
}
