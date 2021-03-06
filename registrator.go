package main

import (
	"errors"
	"fmt"
	"log"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/route53"
	"github.com/miekg/dns"
	"k8s.io/api/extensions/v1beta1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

var (
	errRegistratorMissingOption = errors.New("missing required registrator option")
	errDNSEmptyAnswer           = errors.New("DNS nameserver returned an empty answer")
	defaultResyncPeriod         = 15 * time.Minute
	defaultBatchProcessCycle    = 5 * time.Second
	dnsClient                   = &dns.Client{}
)

type dnsZone interface {
	UpsertCnames(records []cnameRecord) error
	DeleteCnames(records []cnameRecord) error
	Domain() string
	ListNameservers() []string
}

type cnameChange struct {
	Action string
	Record cnameRecord
}

type cnameRecord struct {
	Hostname string
	Target   string
}

type registrator struct {
	dnsZone
	*ingressWatcher
	options     registratorOptions
	sats        []selectorAndTarget
	updateQueue chan cnameChange
}

type registratorOptions struct {
	AWSSessionOptions *session.Options
	KubernetesConfig  *rest.Config
	Targets           []string // required
	TargetLabelName   string   // required
	Route53ZoneID     string   // required
	ResyncPeriod      time.Duration
}

type selectorAndTarget struct {
	Selector labels.Selector
	Target   string
}

func newRegistrator(zoneID string, targets []string, targetLabelName string) (*registrator, error) {
	return newRegistratorWithOptions(
		registratorOptions{
			Route53ZoneID:   zoneID,
			Targets:         targets,
			TargetLabelName: targetLabelName,
		})
}

func newRegistratorWithOptions(options registratorOptions) (*registrator, error) {
	// check required options are set
	if len(options.Targets) == 0 || options.Route53ZoneID == "" || options.TargetLabelName == "" {
		return nil, errRegistratorMissingOption
	}
	var sats []selectorAndTarget
	for _, target := range options.Targets {
		s, err := labels.Parse(options.TargetLabelName + "=" + target)
		if err != nil {
			return nil, err
		}
		sats = append(sats, selectorAndTarget{Selector: s, Target: target})
	}
	if options.AWSSessionOptions == nil {
		options.AWSSessionOptions = &session.Options{}
	}
	if options.KubernetesConfig == nil {
		c, err := rest.InClusterConfig()
		if err != nil {
			return nil, err
		}
		options.KubernetesConfig = c
	}
	if options.ResyncPeriod == 0 {
		options.ResyncPeriod = defaultResyncPeriod
	}
	return &registrator{
		options:     options,
		sats:        sats,
		updateQueue: make(chan cnameChange, 64),
	}, nil
}

func (r *registrator) Start() error {
	sess, err := session.NewSessionWithOptions(*r.options.AWSSessionOptions)
	if err != nil {
		return err
	}
	dns, err := newRoute53Zone(r.options.Route53ZoneID, route53.New(sess))
	if err != nil {
		return err
	}
	r.dnsZone = dns
	log.Println("[INFO] setup route53 session")
	kubeClient, err := kubernetes.NewForConfig(r.options.KubernetesConfig)
	if err != nil {
		return err
	}
	r.ingressWatcher = newIngressWatcher(kubeClient, r.handler, r.options.TargetLabelName, r.options.ResyncPeriod)
	log.Println("[INFO] setup kubernetes ingress watcher")
	wg := sync.WaitGroup{}
	wg.Add(1)
	go func() {
		defer wg.Done()
		r.processUpdateQueue()
	}()
	r.ingressWatcher.Start()
	wg.Wait()
	return nil
}

func (r *registrator) handler(eventType watch.EventType, oldIngress *v1beta1.Ingress, newIngress *v1beta1.Ingress) {
	switch eventType {
	case watch.Added:
		log.Printf("[DEBUG] received %s event for %s", eventType, newIngress.Name)
		metricUpdatesReceived.WithLabelValues(newIngress.Name, "add").Inc()
		hostnames := getHostnamesFromIngress(newIngress)
		target := r.getTargetForIngress(newIngress)
		if target == "" {
			log.Printf("[INFO] invalid ingress target for new ingress %s: %s", newIngress.Name, newIngress.Labels[r.options.TargetLabelName])
		} else if len(hostnames) == 0 {
			log.Printf("[INFO] could not extract hostnames from new ingress %s", newIngress.Name)
		} else {
			log.Printf("[DEBUG] queued update of %d record(s) for new ingress %s, pointing to %s", len(hostnames), newIngress.Name, target)
			r.queueUpdates(route53.ChangeActionUpsert, hostnames, target)
		}
	case watch.Modified:
		log.Printf("[DEBUG] received %s event for %s", eventType, newIngress.Name)
		metricUpdatesReceived.WithLabelValues(newIngress.Name, "modify").Inc()
		newHostnames := getHostnamesFromIngress(newIngress)
		newTarget := r.getTargetForIngress(newIngress)
		oldHostnames := getHostnamesFromIngress(oldIngress)
		oldTarget := r.getTargetForIngress(oldIngress)
		diffHostnames := diffStringSlices(oldHostnames, newHostnames)
		if len(diffHostnames) == 0 && newIngress.Labels[r.options.TargetLabelName] == oldIngress.Labels[r.options.TargetLabelName] {
			log.Printf("[DEBUG] no changes for ingress %s, looks like a no-op resync", newIngress.Name)
			break
		}
		if newTarget == "" {
			log.Printf("[INFO] invalid ingress target for modified ingress %s: %s", newIngress.Name, newIngress.Labels[r.options.TargetLabelName])
		} else if len(newHostnames) == 0 {
			log.Printf("[INFO] could not extract hostnames from modified ingress %s", newIngress.Name)
		} else {
			log.Printf("[DEBUG] queued update of %d record(s) for modified ingress %s, pointing to %s", len(newHostnames), newIngress.Name, newTarget)
			r.queueUpdates(route53.ChangeActionUpsert, newHostnames, newTarget)
		}
		if oldTarget == "" {
			log.Printf("[INFO] invalid ingress target for previous ingress %s: %s", oldIngress.Name, oldIngress.Labels[r.options.TargetLabelName])
		} else if len(diffHostnames) == 0 {
			log.Printf("[DEBUG] no difference in hostnames from previous ingress %s", oldIngress.Name)
		} else {
			log.Printf("[DEBUG] queued deletion of %d record(s) for previous ingress %s", len(diffHostnames), oldIngress.Name)
			r.queueUpdates(route53.ChangeActionDelete, diffHostnames, oldTarget)
		}
	case watch.Deleted:
		log.Printf("[DEBUG] received %s event for %s", eventType, oldIngress.Name)
		metricUpdatesReceived.WithLabelValues(oldIngress.Name, "delete").Inc()
		hostnames := getHostnamesFromIngress(oldIngress)
		target := r.getTargetForIngress(oldIngress)
		if target == "" {
			log.Printf("[INFO] invalid ingress target for old ingress %s: %s", oldIngress.Name, oldIngress.Labels[r.options.TargetLabelName])
		} else if len(hostnames) == 0 {
			log.Printf("[INFO] could not extract hostnames from old ingress %s", oldIngress.Name)
		} else {
			log.Printf("[DEBUG] queued deletion of %d record(s) for old ingress %s", len(hostnames), oldIngress.Name)
			r.queueUpdates(route53.ChangeActionDelete, hostnames, target)
		}
	default:
		log.Printf("[DEBUG] received %s event: cannot handle", eventType)
	}
}

func (r *registrator) queueUpdates(action string, hostnames []string, target string) {
	for _, h := range hostnames {
		r.updateQueue <- cnameChange{action, cnameRecord{h, target}}
	}
}

func (r *registrator) processUpdateQueue() {
	ret := []cnameChange{}
	for {
		select {
		case t := <-r.updateQueue:
			if len(ret) > 0 && ((ret[0].Action == route53.ChangeActionDelete && t.Action != route53.ChangeActionDelete) || (ret[0].Action != route53.ChangeActionDelete && t.Action == route53.ChangeActionDelete)) {
				r.applyBatch(ret)
				ret = []cnameChange{}
			}
			ret = append(ret, t)
		case <-r.stopChannel:
			if len(ret) > 0 {
				r.applyBatch(ret)
				ret = []cnameChange{}
			}
			return
		default:
			if len(ret) > 0 {
				r.applyBatch(ret)
				ret = []cnameChange{}
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
}

func (r *registrator) applyBatch(changes []cnameChange) {
	action := changes[0].Action
	records := make([]cnameRecord, len(changes))
	for i, c := range changes {
		records[i] = c.Record
	}
	pruned := r.pruneBatch(action, records)
	if len(pruned) == 0 {
		return
	}
	hostnames := make([]string, len(pruned))
	for i, p := range pruned {
		hostnames[i] = p.Hostname
	}
	if action == route53.ChangeActionDelete {
		log.Printf("[INFO] deleting %d record(s): %+v", len(pruned), hostnames)
		if !*dryRun {
			if err := r.DeleteCnames(pruned); err != nil {
				log.Printf("[ERROR] error deleting records: %+v", err)
			} else {
				log.Printf("[INFO] records were deleted")
				for _, p := range pruned {
					metricUpdatesApplied.WithLabelValues(p.Hostname, "delete").Inc()
				}
			}
		}
	} else {
		log.Printf("[INFO] modifying %d record(s): %+v", len(pruned), hostnames)
		if !*dryRun {
			if err := r.UpsertCnames(pruned); err != nil {
				log.Printf("[ERROR] error modifying records: %+v", err)
			} else {
				log.Printf("[INFO] records were modified")
				for _, p := range pruned {
					metricUpdatesApplied.WithLabelValues(p.Hostname, "upsert").Inc()
				}
			}
		}
	}
}

func (r *registrator) getTargetForIngress(ingress *v1beta1.Ingress) string {
	for _, sat := range r.sats {
		if sat.Selector.Matches(labels.Set(ingress.Labels)) {
			return sat.Target
		}
	}
	return ""
}

func (r *registrator) pruneBatch(action string, records []cnameRecord) []cnameRecord {
	pruned := []cnameRecord{}
	for _, u := range records {
		if !r.canHandleRecord(u.Hostname) {
			metricUpdatesRejected.Inc()
			log.Printf("[INFO] cannot handle dns record %s, will ignore it", u.Hostname)
			continue
		}
		t, err := resolveCname(fmt.Sprintf("%s.", strings.Trim(u.Hostname, ".")), r.ListNameservers())
		switch action {
		case route53.ChangeActionDelete:
			o := r.ingressWatcher.HostnameOwners(u.Hostname)
			if len(o) > 0 {
				log.Printf("[DEBUG] will not delete record %s because it's still claimed by: %s", u.Hostname, strings.Join(o, ","))
			} else if err == nil {
				pruned = append(pruned, u)
			} else if err != errDNSEmptyAnswer {
				log.Printf("[DEBUG] error resolving %s: %+v, will try to delete the record", u.Hostname, err)
				pruned = append(pruned, u)
			} else {
				log.Printf("[DEBUG] %s does not resolve, no-op", u.Hostname)
			}
		case route53.ChangeActionUpsert:
			if err != nil {
				log.Printf("[DEBUG] error resolving %s: %+v, will try to update the record", u.Hostname, err)
				pruned = append(pruned, u)
			} else if strings.Trim(t, ".") != u.Target {
				pruned = append(pruned, u)
			} else {
				log.Printf("[DEBUG] %s resolves correctly, no-op", u.Hostname)
			}
		}
	}
	pruned = uniqueRecords(pruned)
	return pruned
}

func (r *registrator) canHandleRecord(record string) bool {
	zone := strings.Trim(r.Domain(), ".")
	record = strings.Trim(record, ".")
	matches, err := regexp.MatchString(fmt.Sprintf("^[^.]+\\.%s$", strings.Replace(zone, ".", "\\.", -1)), record)
	if err != nil {
		log.Printf("[DEBUG] regexp match error, will not handle record %s: %+v", record, err)
		return false
	}
	return matches
}

func resolveCname(name string, nameservers []string) (string, error) {
	m := dns.Msg{}
	m.SetQuestion(name, dns.TypeCNAME)
	var retError error
	var retTarget string
	for _, nameserver := range nameservers {
		r, _, err := dnsClient.Exchange(&m, nameserver)
		if err != nil {
			retError = err
			continue
		}
		if len(r.Answer) == 0 {
			retError = errDNSEmptyAnswer
			continue
		}
		retTarget = r.Answer[0].(*dns.CNAME).Target
		retError = nil
		break
	}
	return retTarget, retError
}

func diffStringSlices(a []string, b []string) []string {
	ret := []string{}
	for _, va := range a {
		exists := false
		for _, vb := range b {
			if va == vb {
				exists = true
				break
			}
		}
		if !exists {
			ret = append(ret, va)
		}
	}
	return ret
}

func uniqueRecords(records []cnameRecord) []cnameRecord {
	uniqueRecords := []cnameRecord{}
	rejectedRecords := []string{}
	for i, r1 := range records {
		if stringInSlice(r1.Hostname, rejectedRecords) || recordHostnameInSlice(r1.Hostname, uniqueRecords) {
			continue
		}
		duplicates := []cnameRecord{}
		for j, r2 := range records {
			if i != j && r1.Hostname == r2.Hostname {
				duplicates = append(duplicates, r2)
			}
		}
		if recordTargetsAllMatch(r1.Target, duplicates) {
			uniqueRecords = append(uniqueRecords, r1)
		} else {
			rejectedRecords = append(rejectedRecords, r1.Hostname)
		}
	}
	if len(rejectedRecords) > 0 {
		metricUpdatesRejected.Add(float64(len(rejectedRecords)))
		log.Printf("[INFO] refusing to modify the following records: [%s]: they are claimed by multiple ingresses but are pointing to different targets", strings.Join(rejectedRecords, ", "))
	}
	return uniqueRecords
}

func stringInSlice(s string, slice []string) bool {
	for _, x := range slice {
		if s == x {
			return true
		}
	}
	return false
}

func recordHostnameInSlice(h string, records []cnameRecord) bool {
	for _, x := range records {
		if h == x.Hostname {
			return true
		}
	}
	return false
}

func recordTargetsAllMatch(target string, records []cnameRecord) bool {
	for _, r := range records {
		if target != r.Target {
			return false
		}
	}
	return true
}
