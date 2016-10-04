/*
 * Minio Client (C) 2015 Minio, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package cmd

import (
	"crypto/tls"
	"errors"
	"hash/fnv"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"io/ioutil"

	"github.com/minio/mc/pkg/httptracer"
	"github.com/ricoharisin91/minio-go"
	"github.com/ricoharisin91/minio-go/pkg/policy"
	"github.com/minio/minio/pkg/probe"
)

// S3 client
type s3Client struct {
	mutex        *sync.Mutex
	targetURL    *clientURL
	api          *minio.Client
	virtualStyle bool
}

const (
	amazonHostName = "s3.amazonaws.com"
	googleHostName = "storage.googleapis.com"
)

// newFactory encloses New function with client cache.
func newFactory() func(config *Config) (Client, *probe.Error) {
	clientCache := make(map[uint32]*minio.Client)
	mutex := &sync.Mutex{}

	// Return New function.
	return func(config *Config) (Client, *probe.Error) {
		// Creates a parsed URL.
		targetURL := newClientURL(config.HostURL)
		// By default enable HTTPs.
		secure := true
		if targetURL.Scheme == "http" {
			secure = false
		}

		// Instantiate s3
		s3Clnt := &s3Client{}
		// Allocate a new mutex.
		s3Clnt.mutex = new(sync.Mutex)
		// Save the target URL.
		s3Clnt.targetURL = targetURL

		// Save if target supports virtual host style.
		hostName := targetURL.Host
		s3Clnt.virtualStyle = isVirtualHostStyle(hostName)

		if s3Clnt.virtualStyle {
			// If Amazon URL replace it with 's3.amazonaws.com'
			if isAmazon(hostName) {
				hostName = amazonHostName
			}
			// If Google URL replace it with 'storage.googleapis.com'
			if isGoogle(hostName) {
				hostName = googleHostName
			}
		}

		// Generate a hash out of s3Conf.
		confHash := fnv.New32a()
		confHash.Write([]byte(hostName + config.AccessKey + config.SecretKey))
		confSum := confHash.Sum32()

		// Lookup previous cache by hash.
		mutex.Lock()
		defer mutex.Unlock()
		var api *minio.Client
		var found bool
		if api, found = clientCache[confSum]; !found {
			// Not found. Instantiate a new minio
			var e error
			if strings.ToUpper(config.Signature) == "S3V2" {
				// if Signature version '2' use NewV2 directly.
				api, e = minio.NewV2(hostName, config.AccessKey, config.SecretKey, secure)
			} else {
				// if Signature version '4' use NewV4 directly.
				api, e = minio.NewV4(hostName, config.AccessKey, config.SecretKey, secure)
			}
			if e != nil {
				return nil, probe.NewError(e)
			}
			transport := http.DefaultTransport
			if config.Insecure {
				transport = &http.Transport{
					TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
				}
			}
			if config.Debug {
				if config.Signature == "S3v4" {
					transport = httptracer.GetNewTraceTransport(newTraceV4(), transport)
				}
				if config.Signature == "S3v2" {
					transport = httptracer.GetNewTraceTransport(newTraceV2(), transport)
				}
				// Set custom transport.
			}
			api.SetCustomTransport(transport)
			// Cache the new minio client with hash of config as key.
			clientCache[confSum] = api
		}
		// Set app info.
		api.SetAppInfo(config.AppName, config.AppVersion)

		// Store the new api object.
		s3Clnt.api = api

		return s3Clnt, nil
	}
}

// s3New returns an initialized s3Client structure. If debug is enabled,
// it also enables an internal trace transport.
var s3New = newFactory()

// GetURL get url.
func (c *s3Client) GetURL() clientURL {
	return *c.targetURL
}

// Add bucket notification
func (c *s3Client) AddNotificationConfig(arn string, events []string, prefix, suffix string) *probe.Error {
	bucket, _ := c.url2BucketAndObject()
	if err := isValidBucketName(bucket); err != nil {
		return err
	}

	// Validate total fields in ARN.
	fields := strings.Split(arn, ":")
	if len(fields) != 6 {
		return errInvalidArgument()
	}

	// Get any enabled notification.
	mb, e := c.api.GetBucketNotification(bucket)
	if e != nil {
		return probe.NewError(e)
	}

	accountArn := minio.NewArn(fields[1], fields[2], fields[3], fields[4], fields[5])
	nc := minio.NewNotificationConfig(accountArn)

	// Configure events
	for _, event := range events {
		switch event {
		case "put":
			nc.AddEvents(minio.ObjectCreatedAll)
		case "delete":
			nc.AddEvents(minio.ObjectRemovedAll)
		default:
			return errInvalidArgument().Trace(events...)
		}
	}
	if prefix != "" {
		nc.AddFilterPrefix(prefix)
	}
	if suffix != "" {
		nc.AddFilterSuffix(suffix)
	}

	switch fields[2] {
	case "sns":
		mb.AddTopic(nc)
	case "sqs":
		mb.AddQueue(nc)
	case "lambda":
		mb.AddLambda(nc)
	default:
		return errInvalidArgument().Trace(fields[2])
	}

	// Set the new bucket configuration
	if err := c.api.SetBucketNotification(bucket, mb); err != nil {
		return probe.NewError(err)
	}
	return nil
}

// Remove bucket notification
func (c *s3Client) RemoveNotificationConfig(arn string) *probe.Error {
	bucket, _ := c.url2BucketAndObject()
	if err := isValidBucketName(bucket); err != nil {
		return err
	}

	// Remove all notification configs if arn is empty
	if arn == "" {
		if err := c.api.RemoveAllBucketNotification(bucket); err != nil {
			return probe.NewError(err)
		}
		return nil
	}

	mb, e := c.api.GetBucketNotification(bucket)
	if e != nil {
		return probe.NewError(e)
	}

	fields := strings.Split(arn, ":")
	if len(fields) != 6 {
		return errInvalidArgument().Trace(fields...)
	}
	accountArn := minio.NewArn(fields[1], fields[2], fields[3], fields[4], fields[5])

	switch fields[2] {
	case "sns":
		mb.RemoveTopicByArn(accountArn)
	case "sqs":
		mb.RemoveQueueByArn(accountArn)
	case "lambda":
		mb.RemoveLambdaByArn(accountArn)
	default:
		return errInvalidArgument().Trace(fields[2])
	}

	// Set the new bucket configuration
	if e := c.api.SetBucketNotification(bucket, mb); e != nil {
		return probe.NewError(e)
	}
	return nil
}

type notificationConfig struct {
	ID     string   `json:"id"`
	Arn    string   `json:"arn"`
	Events []string `json:"events"`
	Prefix string   `json:"prefix"`
	Suffix string   `json:"suffix"`
}

// List notification configs
func (c *s3Client) ListNotificationConfigs(arn string) ([]notificationConfig, *probe.Error) {
	var configs []notificationConfig
	bucket, _ := c.url2BucketAndObject()
	if err := isValidBucketName(bucket); err != nil {
		return nil, err
	}

	mb, e := c.api.GetBucketNotification(bucket)
	if e != nil {
		return nil, probe.NewError(e)
	}

	// Generate pretty event names from event types
	prettyEventNames := func(eventsTypes []minio.NotificationEventType) []string {
		var result []string
		for _, eventType := range eventsTypes {
			result = append(result, string(eventType))
		}
		return result
	}

	getFilters := func(config minio.NotificationConfig) (prefix, suffix string) {
		if config.Filter == nil {
			return
		}
		for _, filter := range config.Filter.S3Key.FilterRules {
			if strings.ToLower(filter.Name) == "prefix" {
				prefix = filter.Value
			}
			if strings.ToLower(filter.Name) == "suffix" {
				suffix = filter.Value
			}

		}
		return prefix, suffix
	}

	for _, config := range mb.TopicConfigs {
		if arn != "" && config.Topic != arn {
			continue
		}
		prefix, suffix := getFilters(config.NotificationConfig)
		configs = append(configs, notificationConfig{ID: config.Id,
			Arn:    config.Topic,
			Events: prettyEventNames(config.Events),
			Prefix: prefix,
			Suffix: suffix})
	}

	for _, config := range mb.QueueConfigs {
		if arn != "" && config.Queue != arn {
			continue
		}
		prefix, suffix := getFilters(config.NotificationConfig)
		configs = append(configs, notificationConfig{ID: config.Id,
			Arn:    config.Queue,
			Events: prettyEventNames(config.Events),
			Prefix: prefix,
			Suffix: suffix})
	}

	for _, config := range mb.LambdaConfigs {
		if arn != "" && config.Lambda != arn {
			continue
		}
		prefix, suffix := getFilters(config.NotificationConfig)
		configs = append(configs, notificationConfig{ID: config.Id,
			Arn:    config.Lambda,
			Events: prettyEventNames(config.Events),
			Prefix: prefix,
			Suffix: suffix})
	}

	return configs, nil
}

// Unwatch de-registers all bucket notification events for a given accountID.
func (c *s3Client) Unwatch(params watchParams) *probe.Error {
	// Extract bucket and object.
	bucket, _ := c.url2BucketAndObject()
	if err := isValidBucketName(bucket); err != nil {
		return err
	}
	// Success.
	return nil
}

// Start watching on all bucket events for a given account ID.
func (c *s3Client) Watch(params watchParams) (*watchObject, *probe.Error) {
	eventChan := make(chan Event)
	errorChan := make(chan *probe.Error)
	doneChan := make(chan bool)

	// Extract bucket and object.
	bucket, object := c.url2BucketAndObject()
	if err := isValidBucketName(bucket); err != nil {
		return nil, err
	}

	// Flag set to set the notification.
	var events []string
	for _, event := range params.events {
		switch event {
		case "put":
			events = append(events, string(minio.ObjectCreatedAll))
		case "delete":
			events = append(events, string(minio.ObjectRemovedAll))
		default:
			return nil, errInvalidArgument().Trace(event)
		}
	}
	if object != "" && params.prefix != "" {
		return nil, errInvalidArgument().Trace(params.prefix, object)
	}
	if object != "" && params.prefix == "" {
		params.prefix = object
	}

	doneCh := make(chan struct{})

	// wait for doneChan to close the other channels
	go func() {
		<-doneChan

		close(doneCh)
		close(eventChan)
		close(errorChan)
	}()

	// Start listening on all bucket events.
	eventsCh := c.api.ListenBucketNotification(bucket, params.prefix, params.suffix, events, doneCh)

	// wait for events to occur and sent them through the eventChan and errorChan
	go func() {
		for notificationInfo := range eventsCh {
			if notificationInfo.Err != nil {
				errorChan <- probe.NewError(notificationInfo.Err)
				continue
			}

			for _, record := range notificationInfo.Records {
				bucketName := record.S3.Bucket.Name
				key, e := url.QueryUnescape(record.S3.Object.Key)
				if e != nil {
					errorChan <- probe.NewError(e)
					continue
				}

				u := *c.targetURL
				u.Path = path.Join(string(u.Separator), bucketName, key)
				if strings.HasPrefix(record.EventName, "s3:ObjectCreated:") {
					eventChan <- Event{
						Time:   record.EventTime,
						Size:   record.S3.Object.Size,
						Path:   u.String(),
						Client: c,
						Type:   EventCreate,
					}
				} else if strings.HasPrefix(record.EventName, "s3:ObjectRemoved:") {
					eventChan <- Event{
						Time:   record.EventTime,
						Path:   u.String(),
						Client: c,
						Type:   EventRemove,
					}
				} else {
					// ignore other events
				}
			}
		}
	}()

	return &watchObject{
		events: eventChan,
		errors: errorChan,
		done:   doneChan,
	}, nil
}

// Get - get object.
func (c *s3Client) Get() (io.Reader, *probe.Error) {
	bucket, object := c.url2BucketAndObject()
	reader, e := c.api.GetObject(bucket, object)
	if e != nil {
		errResponse := minio.ToErrorResponse(e)
		if errResponse.Code == "AccessDenied" {
			return nil, probe.NewError(PathInsufficientPermission{Path: c.targetURL.String()})
		}
		if errResponse.Code == "NoSuchBucket" {
			return nil, probe.NewError(BucketDoesNotExist{
				Bucket: bucket,
			})
		}
		if errResponse.Code == "InvalidBucketName" {
			return nil, probe.NewError(BucketInvalid{
				Bucket: bucket,
			})
		}
		if errResponse.Code == "NoSuchKey" || errResponse.Code == "InvalidArgument" {
			return nil, probe.NewError(ObjectMissing{})
		}
		return nil, probe.NewError(e)
	}
	return reader, nil
}

// Copy - copy object
func (c *s3Client) Copy(source string, size int64, progress io.Reader) *probe.Error {
	bucket, object := c.url2BucketAndObject()
	if bucket == "" {
		return probe.NewError(BucketNameEmpty{})
	}
	// Empty copy conditions
	copyConds := minio.NewCopyConditions()
	e := c.api.CopyObject(bucket, object, source, copyConds)
	if e != nil {
		errResponse := minio.ToErrorResponse(e)
		if errResponse.Code == "AccessDenied" {
			return probe.NewError(PathInsufficientPermission{
				Path: c.targetURL.String(),
			})
		}
		if errResponse.Code == "NoSuchBucket" {
			return probe.NewError(BucketDoesNotExist{
				Bucket: bucket,
			})
		}
		if errResponse.Code == "InvalidBucketName" {
			return probe.NewError(BucketInvalid{
				Bucket: bucket,
			})
		}
		if errResponse.Code == "NoSuchKey" || errResponse.Code == "InvalidArgument" {
			return probe.NewError(ObjectMissing{})
		}
		return probe.NewError(e)
	}
	// Successful copy update progress bar if there is one.
	if progress != nil {
		if _, e := io.CopyN(ioutil.Discard, progress, size); e != nil {
			return probe.NewError(e)
		}
	}
	return nil
}

// Put - put object.
func (c *s3Client) Put(reader io.Reader, size int64, contentType string, progress io.Reader) (int64, *probe.Error) {
	// md5 is purposefully ignored since AmazonS3 does not return proper md5sum
	// for a multipart upload and there is no need to cross verify,
	// invidual parts are properly verified fully in transit and also upon completion
	// of the multipart request.
	bucket, object := c.url2BucketAndObject()
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	if bucket == "" {
		return 0, probe.NewError(BucketNameEmpty{})
	}
	n, e := c.api.PutObjectWithProgress(bucket, object, reader, contentType, progress)
	if e != nil {
		errResponse := minio.ToErrorResponse(e)
		if errResponse.Code == "UnexpectedEOF" || e == io.EOF {
			return n, probe.NewError(UnexpectedEOF{
				TotalSize:    size,
				TotalWritten: n,
			})
		}
		if errResponse.Code == "AccessDenied" {
			return n, probe.NewError(PathInsufficientPermission{
				Path: c.targetURL.String(),
			})
		}
		if errResponse.Code == "MethodNotAllowed" {
			return n, probe.NewError(ObjectAlreadyExists{
				Object: object,
			})
		}
		if errResponse.Code == "XMinioObjectExistsAsDirectory" {
			return n, probe.NewError(ObjectAlreadyExistsAsDirectory{
				Object: object,
			})
		}
		if errResponse.Code == "NoSuchBucket" {
			return n, probe.NewError(BucketDoesNotExist{
				Bucket: bucket,
			})
		}
		if errResponse.Code == "InvalidBucketName" {
			return n, probe.NewError(BucketInvalid{
				Bucket: bucket,
			})
		}
		if errResponse.Code == "NoSuchKey" || errResponse.Code == "InvalidArgument" {
			return n, probe.NewError(ObjectMissing{})
		}
		return n, probe.NewError(e)
	}
	return n, nil
}

// Remove - remove object or bucket.
func (c *s3Client) Remove(incomplete bool) *probe.Error {
	bucket, object := c.url2BucketAndObject()
	// Remove only incomplete object.
	if incomplete && object != "" {
		e := c.api.RemoveIncompleteUpload(bucket, object)
		return probe.NewError(e)
	}
	var e error
	if object == "" {
		e = c.api.RemoveBucket(bucket)
	} else {
		e = c.api.RemoveObject(bucket, object)
	}
	return probe.NewError(e)
}

// We support '.' with bucket names but we fallback to using path
// style requests instead for such buckets
var validBucketName = regexp.MustCompile(`^[a-z0-9][a-z0-9\.\-]{1,61}[a-z0-9]$`)

// isValidBucketName - verify bucket name in accordance with
//  - http://docs.aws.amazon.com/AmazonS3/latest/dev/UsingBucket.html
func isValidBucketName(bucketName string) *probe.Error {
	if strings.TrimSpace(bucketName) == "" {
		return probe.NewError(errors.New("Bucket name cannot be empty."))
	}
	if len(bucketName) < 3 || len(bucketName) > 63 {
		return probe.NewError(errors.New("Bucket name should be more than 3 characters and less than 64 characters"))
	}
	if !validBucketName.MatchString(bucketName) {
		return probe.NewError(errors.New("Bucket name can contain alphabet, '-' and numbers, but first character should be an alphabet or number"))
	}
	return nil
}

// MakeBucket - make a new bucket.
func (c *s3Client) MakeBucket(region string) *probe.Error {
	bucket, object := c.url2BucketAndObject()
	if object != "" {
		return probe.NewError(BucketNameTopLevel{})
	}
	if err := isValidBucketName(bucket); err != nil {
		return err.Trace(bucket)
	}
	e := c.api.MakeBucket(bucket, region)
	if e != nil {
		return probe.NewError(e)
	}
	return nil
}

// GetAccessRules - get configured policies from the server
func (c *s3Client) GetAccessRules() (map[string]string, *probe.Error) {
	bucket, object := c.url2BucketAndObject()
	if bucket == "" {
		return map[string]string{}, probe.NewError(BucketNameEmpty{})
	}
	policies := map[string]string{}
	policyRules, err := c.api.ListBucketPolicies(bucket, object)
	if err != nil {
		return nil, probe.NewError(err)
	}
	// Hide policy data structure at this level
	for k, v := range policyRules {
		policies[k] = string(v)
	}
	return policies, nil
}

// GetAccess get access policy permissions.
func (c *s3Client) GetAccess() (string, *probe.Error) {
	bucket, object := c.url2BucketAndObject()
	if bucket == "" {
		return "", probe.NewError(BucketNameEmpty{})
	}
	bucketPolicy, e := c.api.GetBucketPolicy(bucket, object)
	if e != nil {
		return "", probe.NewError(e)
	}
	return string(bucketPolicy), nil
}

// SetAccess set access policy permissions.
func (c *s3Client) SetAccess(bucketPolicy string) *probe.Error {
	bucket, object := c.url2BucketAndObject()
	if bucket == "" {
		return probe.NewError(BucketNameEmpty{})
	}
	e := c.api.SetBucketPolicy(bucket, object, policy.BucketPolicy(bucketPolicy))
	if e != nil {
		return probe.NewError(e)
	}
	return nil
}

// listObjectWrapper - select ObjectList version depending on the target hostname
func (c *s3Client) listObjectWrapper(bucket, object string, isRecursive bool, doneCh chan struct{}) <-chan minio.ObjectInfo {
	if c.targetURL.Host == amazonHostName {
		return c.api.ListObjectsV2(bucket, object, isRecursive, doneCh)
	}
	return c.api.ListObjects(bucket, object, isRecursive, doneCh)
}

// Stat - send a 'HEAD' on a bucket or object to fetch its metadata.
func (c *s3Client) Stat() (*clientContent, *probe.Error) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	objectMetadata := &clientContent{}
	bucket, object := c.url2BucketAndObject()
	// Bucket name cannot be empty, stat on URL has no meaning.
	if bucket == "" {
		return nil, probe.NewError(BucketNameEmpty{})
	} else if object == "" {
		exists, e := c.api.BucketExists(bucket)
		if e != nil {
			return nil, probe.NewError(e)
		}
		if !exists {
			return nil, probe.NewError(BucketDoesNotExist{Bucket: bucket})
		}
		bucketMetadata := &clientContent{}
		bucketMetadata.URL = *c.targetURL
		bucketMetadata.Type = os.ModeDir
		return bucketMetadata, nil
	}
	isRecursive := false

	// Remove trailing slashes needed for the following ListObjects call.
	// In addition, Stat() will be as smart as the client fs version and will
	// facilitate the work of the upper layers
	object = strings.TrimRight(object, string(c.targetURL.Separator))

	for objectStat := range c.listObjectWrapper(bucket, object, isRecursive, nil) {
		if objectStat.Err != nil {
			return nil, probe.NewError(objectStat.Err)
		}
		if objectStat.Key == object {
			objectMetadata.URL = *c.targetURL
			objectMetadata.Time = objectStat.LastModified
			objectMetadata.Size = objectStat.Size
			objectMetadata.Type = os.FileMode(0664)
			return objectMetadata, nil
		}
		if strings.HasSuffix(objectStat.Key, string(c.targetURL.Separator)) {
			objectMetadata.URL = *c.targetURL
			objectMetadata.Type = os.ModeDir
			return objectMetadata, nil
		}
	}
	return nil, probe.NewError(ObjectMissing{})
}

func isAmazon(host string) bool {
	matchAmazon, _ := filepath.Match("*.s3*.amazonaws.com", host)
	return matchAmazon
}

func isGoogle(host string) bool {
	matchGoogle, _ := filepath.Match("*.storage.googleapis.com", host)
	return matchGoogle
}

// Figure out if the URL is of 'virtual host' style.
// Currently only supported hosts with virtual style are Amazon S3 and Google Cloud Storage.
func isVirtualHostStyle(host string) bool {
	return isAmazon(host) || isGoogle(host)
}

// url2BucketAndObject gives bucketName and objectName from URL path.
func (c *s3Client) url2BucketAndObject() (bucketName, objectName string) {
	path := c.targetURL.Path
	// Convert any virtual host styled requests.
	//
	// For the time being this check is introduced for S3,
	// If you have custom virtual styled hosts please.
	// List them below.
	if c.virtualStyle {
		var bucket string
		hostIndex := strings.Index(c.targetURL.Host, "s3")
		if hostIndex == -1 {
			hostIndex = strings.Index(c.targetURL.Host, "storage.googleapis")
		}
		if hostIndex > 0 {
			bucket = c.targetURL.Host[:hostIndex-1]
			path = string(c.targetURL.Separator) + bucket + c.targetURL.Path
		}
	}
	splits := strings.SplitN(path, string(c.targetURL.Separator), 3)
	switch len(splits) {
	case 0, 1:
		bucketName = ""
		objectName = ""
	case 2:
		bucketName = splits[1]
		objectName = ""
	case 3:
		bucketName = splits[1]
		objectName = splits[2]
	}
	return bucketName, objectName
}

/// Bucket API operations.

// List - list at delimited path, if not recursive.
func (c *s3Client) List(recursive, incomplete bool) <-chan *clientContent {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	contentCh := make(chan *clientContent)
	if incomplete {
		if recursive {
			go c.listIncompleteRecursiveInRoutine(contentCh)
		} else {
			go c.listIncompleteInRoutine(contentCh)
		}
	} else {
		if recursive {
			go c.listRecursiveInRoutine(contentCh)
		} else {
			go c.listInRoutine(contentCh)
		}
	}
	return contentCh
}

func (c *s3Client) listIncompleteInRoutine(contentCh chan *clientContent) {
	defer close(contentCh)
	// get bucket and object from URL.
	b, o := c.url2BucketAndObject()
	switch {
	case b == "" && o == "":
		buckets, err := c.api.ListBuckets()
		if err != nil {
			contentCh <- &clientContent{
				Err: probe.NewError(err),
			}
			return
		}
		isRecursive := false
		for _, bucket := range buckets {
			for object := range c.api.ListIncompleteUploads(bucket.Name, o, isRecursive, nil) {
				if object.Err != nil {
					contentCh <- &clientContent{
						Err: probe.NewError(object.Err),
					}
					return
				}
				content := &clientContent{}
				url := *c.targetURL
				// Join bucket with - incoming object key.
				url.Path = filepath.Join(string(url.Separator), bucket.Name, object.Key)
				if c.virtualStyle {
					url.Path = filepath.Join(string(url.Separator), object.Key)
				}
				switch {
				case strings.HasSuffix(object.Key, string(c.targetURL.Separator)):
					// We need to keep the trailing Separator, do not use filepath.Join().
					content.URL = url
					content.Time = time.Now()
					content.Type = os.ModeDir
				default:
					content.URL = url
					content.Size = object.Size
					content.Time = object.Initiated
					content.Type = os.ModeTemporary
				}
				contentCh <- content
			}
		}
	default:
		isRecursive := false
		for object := range c.api.ListIncompleteUploads(b, o, isRecursive, nil) {
			if object.Err != nil {
				contentCh <- &clientContent{
					Err: probe.NewError(object.Err),
				}
				return
			}
			content := &clientContent{}
			url := *c.targetURL
			// Join bucket with - incoming object key.
			url.Path = filepath.Join(string(url.Separator), b, object.Key)
			if c.virtualStyle {
				url.Path = filepath.Join(string(url.Separator), object.Key)
			}
			switch {
			case strings.HasSuffix(object.Key, string(c.targetURL.Separator)):
				// We need to keep the trailing Separator, do not use filepath.Join().
				content.URL = url
				content.Time = time.Now()
				content.Type = os.ModeDir
			default:
				content.URL = url
				content.Size = object.Size
				content.Time = object.Initiated
				content.Type = os.ModeTemporary
			}
			contentCh <- content
		}
	}
}

func (c *s3Client) listIncompleteRecursiveInRoutine(contentCh chan *clientContent) {
	defer close(contentCh)
	// get bucket and object from URL.
	b, o := c.url2BucketAndObject()
	switch {
	case b == "" && o == "":
		buckets, err := c.api.ListBuckets()
		if err != nil {
			contentCh <- &clientContent{
				Err: probe.NewError(err),
			}
			return
		}
		isRecursive := true
		for _, bucket := range buckets {
			for object := range c.api.ListIncompleteUploads(bucket.Name, o, isRecursive, nil) {
				if object.Err != nil {
					contentCh <- &clientContent{
						Err: probe.NewError(object.Err),
					}
					return
				}
				url := *c.targetURL
				url.Path = filepath.Join(url.Path, bucket.Name, object.Key)
				content := &clientContent{}
				content.URL = url
				content.Size = object.Size
				content.Time = object.Initiated
				content.Type = os.ModeTemporary
				contentCh <- content
			}
		}
	default:
		isRecursive := true
		for object := range c.api.ListIncompleteUploads(b, o, isRecursive, nil) {
			if object.Err != nil {
				contentCh <- &clientContent{
					Err: probe.NewError(object.Err),
				}
				return
			}
			url := *c.targetURL
			// Join bucket and incoming object key.
			url.Path = filepath.Join(string(url.Separator), b, object.Key)
			if c.virtualStyle {
				url.Path = filepath.Join(string(url.Separator), object.Key)
			}
			content := &clientContent{}
			content.URL = url
			content.Size = object.Size
			content.Time = object.Initiated
			content.Type = os.ModeTemporary
			contentCh <- content
		}
	}
}

func (c *s3Client) listInRoutine(contentCh chan *clientContent) {
	defer close(contentCh)
	// get bucket and object from URL.
	b, o := c.url2BucketAndObject()
	switch {
	case b == "" && o == "":
		buckets, e := c.api.ListBuckets()
		if e != nil {
			contentCh <- &clientContent{
				Err: probe.NewError(e),
			}
			return
		}
		for _, bucket := range buckets {
			url := *c.targetURL
			url.Path = filepath.Join(url.Path, bucket.Name)
			content := &clientContent{}
			content.URL = url
			content.Size = 0
			content.Time = bucket.CreationDate
			content.Type = os.ModeDir
			contentCh <- content
		}
	case b != "" && !strings.HasSuffix(c.targetURL.Path, string(c.targetURL.Separator)) && o == "":
		buckets, e := c.api.ListBuckets()
		if e != nil {
			contentCh <- &clientContent{
				Err: probe.NewError(e),
			}
		}
		for _, bucket := range buckets {
			if bucket.Name == b {
				content := &clientContent{}
				content.URL = *c.targetURL
				content.Size = 0
				content.Time = bucket.CreationDate
				content.Type = os.ModeDir
				contentCh <- content
				break
			}
		}
	default:
		isRecursive := false
		for object := range c.listObjectWrapper(b, o, isRecursive, nil) {
			if object.Err != nil {
				contentCh <- &clientContent{
					Err: probe.NewError(object.Err),
				}
				return
			}
			content := &clientContent{}
			url := *c.targetURL
			// Join bucket and incoming object key.
			url.Path = filepath.Join(string(url.Separator), b, object.Key)
			if c.virtualStyle {
				url.Path = filepath.Join(string(url.Separator), object.Key)
			}
			switch {
			case strings.HasSuffix(object.Key, string(c.targetURL.Separator)):
				// We need to keep the trailing Separator, do not use filepath.Join().
				content.URL = url
				content.Time = time.Now()
				content.Type = os.ModeDir
			default:
				content.URL = url
				content.Size = object.Size
				content.Time = object.LastModified
				content.Type = os.FileMode(0664)
			}
			contentCh <- content
		}
	}
}

// S3 offers a range of storage classes designed for
// different use cases, following list captures these.
const (
	// General purpose.
	// s3StorageClassStandard = "STANDARD"
	// Infrequent access.
	// s3StorageClassInfrequent = "STANDARD_IA"
	// Reduced redundancy access.
	// s3StorageClassRedundancy = "REDUCED_REDUNDANCY"
	// Archive access.
	s3StorageClassGlacier = "GLACIER"
)

func (c *s3Client) listRecursiveInRoutine(contentCh chan *clientContent) {
	defer close(contentCh)
	// get bucket and object from URL.
	b, o := c.url2BucketAndObject()
	switch {
	case b == "" && o == "":
		buckets, err := c.api.ListBuckets()
		if err != nil {
			contentCh <- &clientContent{
				Err: probe.NewError(err),
			}
			return
		}
		for _, bucket := range buckets {
			bucketURL := *c.targetURL
			bucketURL.Path = filepath.Join(bucketURL.Path, bucket.Name)
			contentCh <- &clientContent{
				URL:  bucketURL,
				Type: os.ModeDir,
				Time: bucket.CreationDate,
			}
			isRecursive := true
			for object := range c.listObjectWrapper(bucket.Name, o, isRecursive, nil) {
				// Return error if we encountered glacier object and continue.
				if object.StorageClass == s3StorageClassGlacier {
					contentCh <- &clientContent{
						Err: probe.NewError(ObjectOnGlacier{object.Key}),
					}
					continue
				}
				if object.Err != nil {
					contentCh <- &clientContent{
						Err: probe.NewError(object.Err),
					}
					continue
				}
				content := &clientContent{}
				objectURL := *c.targetURL
				objectURL.Path = filepath.Join(objectURL.Path, bucket.Name, object.Key)
				content.URL = objectURL
				content.Size = object.Size
				content.Time = object.LastModified
				content.Type = os.FileMode(0664)
				contentCh <- content
			}
		}
	default:
		isRecursive := true
		for object := range c.listObjectWrapper(b, o, isRecursive, nil) {
			if object.Err != nil {
				contentCh <- &clientContent{
					Err: probe.NewError(object.Err),
				}
				continue
			}
			// Ignore S3 empty directories
			if object.Size == 0 && strings.HasSuffix(object.Key, "/") {
				continue
			}
			content := &clientContent{}
			url := *c.targetURL
			// Join bucket and incoming object key.
			url.Path = filepath.Join(string(url.Separator), b, object.Key)
			// If virtualStyle replace the url.Path back.
			if c.virtualStyle {
				url.Path = filepath.Join(string(url.Separator), object.Key)
			}
			content.URL = url
			content.Size = object.Size
			content.Time = object.LastModified
			content.Type = os.FileMode(0664)
			contentCh <- content
		}
	}
}

// ShareDownload - get a usable presigned object url to share.
func (c *s3Client) ShareDownload(expires time.Duration) (string, *probe.Error) {
	bucket, object := c.url2BucketAndObject()
	// No additional request parameters are set for the time being.
	reqParams := make(url.Values)
	presignedURL, e := c.api.PresignedGetObject(bucket, object, expires, reqParams)
	if e != nil {
		return "", probe.NewError(e)
	}
	return presignedURL.String(), nil
}

// ShareUpload - get data for presigned post http form upload.
func (c *s3Client) ShareUpload(isRecursive bool, expires time.Duration, contentType string) (map[string]string, *probe.Error) {
	bucket, object := c.url2BucketAndObject()
	p := minio.NewPostPolicy()
	if e := p.SetExpires(time.Now().UTC().Add(expires)); e != nil {
		return nil, probe.NewError(e)
	}
	if strings.TrimSpace(contentType) != "" || contentType != "" {
		// No need to verify for error here, since we have stripped out spaces.
		p.SetContentType(contentType)
	}
	if e := p.SetBucket(bucket); e != nil {
		return nil, probe.NewError(e)
	}
	if isRecursive {
		if e := p.SetKeyStartsWith(object); e != nil {
			return nil, probe.NewError(e)
		}
	} else {
		if e := p.SetKey(object); e != nil {
			return nil, probe.NewError(e)
		}
	}
	_, m, e := c.api.PresignedPostPolicy(p)
	return m, probe.NewError(e)
}
