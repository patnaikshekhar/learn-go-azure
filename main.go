package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/profiles/2017-03-09/resources/mgmt/subscriptions"
	"github.com/Azure/azure-sdk-for-go/profiles/2019-03-01/resources/mgmt/insights"
	"github.com/Azure/azure-sdk-for-go/services/resources/mgmt/2019-05-01/resources"
	"github.com/Azure/go-autorest/autorest"
	"github.com/Azure/go-autorest/autorest/to"
)

// AzureResource Parts of azure resource identification
type AzureResource struct {
	Subscription  string
	ResourceGroup string
	Provider      string
	Type          string
	Name          string
	SubType       string
	SubName       string
}

var (
	providers = map[string]string{}
	tenant    = ""
	start     = time.Now().AddDate(0, 0, -89)
)

const (
	noOfWorkers = 5
)

func main() {

	// Create main context
	ctx := context.TODO()

	// create an authorizer from env vars or Azure Managed Service Idenity
	log.Println("Starting app Press CTRL+C to end.")
	authorizer, err := newAuthorizer()
	if err != nil || authorizer == nil {
		log.Fatalf("Impossible to authenticate %#v", err)
	}
	graphAuthorizer, err := newGraphAuthorizer()
	if err != nil || authorizer == nil {
		log.Fatalf("Impossible to authenticate to graph %#v", err)
	}
	var interval = 300
	intervalSrt, intervalConfigured := os.LookupEnv("CHECK_SECONDS_INTERVAL")
	if intervalConfigured {
		interval, err = strconv.Atoi(intervalSrt)
		if err != nil {
			log.Println("CHECK_SECONDS_INTERVAL is not a valid integer")
			interval = 300
		}
	}
	tenantsClient := subscriptions.NewTenantsClient()
	tenantsClient.Authorizer = *authorizer
	tenants, err := tenantsClient.ListComplete(ctx)
	for tenants.NotDone() {
		value := tenants.Value()
		tenant = *value.TenantID
		tenants.Next()
	}
	subs, err := getSubscriptions(*authorizer)
	log.Printf("Subscriptions are %v", subs)
	providersClient := resources.NewProvidersClient(subs[0])
	providersClient.Authorizer = *authorizer
	providersList, err := providersClient.ListComplete(ctx, to.Int32Ptr(50000), "")
	for providersList.NotDone() {
		value := providersList.Value()
		for _, providerType := range *value.ResourceTypes {
			name := fmt.Sprintf("%s/%s", *value.Namespace, *providerType.ResourceType)
			providers[strings.ToLower(name)] = (*providerType.APIVersions)[0]
		}
		providersList.Next()
	}
	executeUpdates(ctx, interval, authorizer, graphAuthorizer)
	log.Println("End of schedule")
}

// Method focus of this exercise
func executeUpdates(ctx context.Context, interval int, authorizer *autorest.Authorizer, graphAuthorizer *autorest.Authorizer) {

	ticker := time.NewTicker(time.Duration(interval * 1e+9))

	for {
		select {
		case <-ticker.C:

			now := time.Now()

			// Create a work channel
			workerChannel := make(chan string)
			workDoneChannel := make(chan struct{})

			// Start Workers
			for i := 0; i < noOfWorkers; i++ {
				go subscriptionWorker(workerChannel, workDoneChannel, authorizer, graphAuthorizer, start, now)
			}

			subs, err := getSubscriptions(*authorizer)
			if err != nil {
				log.Panic(err)
			}

			for _, sub := range subs {
				// Pass message to worker
				workerChannel <- sub
			}

			for range subs {
				<-workDoneChannel
			}

			log.Printf("All subscriptions completed")

			// Close channel once completed
			close(workerChannel)

			back, _ := time.ParseDuration(fmt.Sprintf("-%ds", interval*20))
			start = now.Add(back)
		case <-ctx.Done():
			return
		}
	}

}

func subscriptionWorker(workerChannel <-chan string, workDoneChannel chan<- struct{}, authorizer *autorest.Authorizer, graphAuthorizer *autorest.Authorizer, start time.Time, now time.Time) {
	// Do work
	for sub := range workerChannel {
		// Execute work sync
		evaluateStatus(*authorizer, *graphAuthorizer, sub, start, now)
		workDoneChannel <- struct{}{}
	}
}

func evaluateStatus(
	auth autorest.Authorizer, authGraph autorest.Authorizer,
	subscription string,
	fromTime time.Time, toTime time.Time) {

	log.Printf("Evaluating status for: %s", subscription)

	resourceClient := resources.NewClient(subscription)
	activityClient := insights.NewActivityLogsClient(subscription)
	activityClient.Authorizer = auth
	resourceClient.Authorizer = auth

	tstarts := fromTime.Format("2006-01-02T15:04:05")
	ts := toTime.Format("2006-01-02T15:04:05")
	filterString := fmt.Sprintf("eventTimestamp ge '%s' and eventTimestamp le '%s'", tstarts, ts)
	listResources, err := activityClient.ListComplete(context.Background(), filterString, "")
	if err != nil {
		log.Fatal(err)
	}
	for listResources.NotDone() {
		logActivity := listResources.Value()
		listResources.Next()
		if logActivity.Caller == nil || logActivity.ResourceType == nil ||
			logActivity.ResourceType.Value == nil ||
			*logActivity.ResourceType.Value == "Microsoft.Resources/deployments" ||
			unsuportedProviders[strings.ToLower(*logActivity.ResourceType.Value)] ||
			logActivity.SubStatus == nil || logActivity.SubStatus.Value == nil ||
			(*logActivity.SubStatus.Value != "Created" && !writeOperation.MatchString(*logActivity.OperationName.Value)) {
			continue
		}
		resourceID := *logActivity.ResourceID
		apiVersion := providers[strings.ToLower(*logActivity.ResourceType.Value)]
		if apiVersion == "" {
			log.Println(strings.ToLower(*logActivity.ResourceType.Value))
			continue
		}
		res, err := resourceClient.GetByID(context.Background(), resourceID, apiVersion)

		if res.Response.StatusCode != 404 && err != nil {
			log.Println("REAL ERROR", err)
			continue
		} else if res.Response.StatusCode == 404 {
			continue
		}

		resID := getResource(*res.ID)

		if res.Tags["Created-by"] == nil {
			if res.Tags == nil {
				res.Tags = map[string]*string{}
			}
			name := "UNKNOWN"
			if logActivity.Claims["name"] != nil {
				name = fmt.Sprintf("%s", *logActivity.Caller)
			} else if logActivity.Claims["appid"] != nil {
				appName, _ := getAppName(logActivity.Claims["appid"], authGraph)
				name = fmt.Sprintf("%s", appName)
			}
			log.Printf("UPDATING %s | %s | %s | %s", resID.Subscription, resID.Name, strings.ToLower(*logActivity.ResourceType.Value), name)
			res.Tags["Created-by"] = to.StringPtr(name)
			res.Tags["Created-by-id"] = logActivity.Caller
			resUpdate := resources.GenericResource{
				ID:   res.ID,
				Tags: res.Tags,
			}
			_, err := resourceClient.UpdateByID(context.Background(), *resUpdate.ID, apiVersion, resUpdate)
			if err != nil {
				log.Println(err)
			}
		}
	}
}
