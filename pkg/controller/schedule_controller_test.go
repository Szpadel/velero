/*
Copyright The Velero Contributors.

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

package controller

import (
	"testing"
	"time"

	"github.com/robfig/cron"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/clock"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	velerov1 "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"
	velerov1api "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"
	"github.com/vmware-tanzu/velero/pkg/builder"
	"github.com/vmware-tanzu/velero/pkg/metrics"
	velerotest "github.com/vmware-tanzu/velero/pkg/test"
)

func TestReconcileOfSchedule(t *testing.T) {
	require.Nil(t, velerov1.AddToScheme(scheme.Scheme))

	newScheduleBuilder := func(phase velerov1api.SchedulePhase) *builder.ScheduleBuilder {
		return builder.ForSchedule("ns", "name").Phase(phase)
	}

	tests := []struct {
		name                     string
		scheduleKey              string
		schedule                 *velerov1api.Schedule
		fakeClockTime            string
		expectedPhase            string
		expectedValidationErrors []string
		expectedBackupCreate     *velerov1api.Backup
		expectedLastBackup       string
	}{
		{
			name:        "missing schedule triggers no backup",
			scheduleKey: "foo/bar",
		},
		{
			name:     "schedule with phase FailedValidation triggers no backup",
			schedule: newScheduleBuilder(velerov1api.SchedulePhaseFailedValidation).Result(),
		},
		{
			name:                     "schedule with phase New gets validated and failed if invalid",
			schedule:                 newScheduleBuilder(velerov1api.SchedulePhaseNew).Result(),
			expectedPhase:            string(velerov1api.SchedulePhaseFailedValidation),
			expectedValidationErrors: []string{"Schedule must be a non-empty valid Cron expression"},
		},
		{
			name:                     "schedule with phase <blank> gets validated and failed if invalid",
			schedule:                 newScheduleBuilder(velerov1api.SchedulePhase("")).Result(),
			expectedPhase:            string(velerov1api.SchedulePhaseFailedValidation),
			expectedValidationErrors: []string{"Schedule must be a non-empty valid Cron expression"},
		},
		{
			name:                     "schedule with phase Enabled gets re-validated and failed if invalid",
			schedule:                 newScheduleBuilder(velerov1api.SchedulePhaseEnabled).Result(),
			expectedPhase:            string(velerov1api.SchedulePhaseFailedValidation),
			expectedValidationErrors: []string{"Schedule must be a non-empty valid Cron expression"},
		},
		{
			name:                 "schedule with phase New gets validated and triggers a backup",
			schedule:             newScheduleBuilder(velerov1api.SchedulePhaseNew).CronSchedule("@every 5m").Result(),
			fakeClockTime:        "2017-01-01 12:00:00",
			expectedPhase:        string(velerov1api.SchedulePhaseEnabled),
			expectedBackupCreate: builder.ForBackup("ns", "name-20170101120000").ObjectMeta(builder.WithLabels(velerov1api.ScheduleNameLabel, "name")).Result(),
			expectedLastBackup:   "2017-01-01 12:00:00",
		},
		{
			name:                 "schedule with phase Enabled gets re-validated and triggers a backup if valid",
			schedule:             newScheduleBuilder(velerov1api.SchedulePhaseEnabled).CronSchedule("@every 5m").Result(),
			fakeClockTime:        "2017-01-01 12:00:00",
			expectedPhase:        string(velerov1api.SchedulePhaseEnabled),
			expectedBackupCreate: builder.ForBackup("ns", "name-20170101120000").ObjectMeta(builder.WithLabels(velerov1api.ScheduleNameLabel, "name")).Result(),
			expectedLastBackup:   "2017-01-01 12:00:00",
		},
		{
			name:                 "schedule that's already run gets LastBackup updated",
			schedule:             newScheduleBuilder(velerov1api.SchedulePhaseEnabled).CronSchedule("@every 5m").LastBackupTime("2000-01-01 00:00:00").Result(),
			fakeClockTime:        "2017-01-01 12:00:00",
			expectedBackupCreate: builder.ForBackup("ns", "name-20170101120000").ObjectMeta(builder.WithLabels(velerov1api.ScheduleNameLabel, "name")).Result(),
			expectedLastBackup:   "2017-01-01 12:00:00",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var (
				client   = (&fake.ClientBuilder{}).Build()
				logger   = velerotest.NewLogger()
				testTime time.Time
				err      error
			)

			reconciler := NewScheduleReconciler("namespace", logger, client, metrics.NewServerMetrics())

			if test.fakeClockTime != "" {
				testTime, err = time.Parse("2006-01-02 15:04:05", test.fakeClockTime)
				require.NoError(t, err, "unable to parse test.fakeClockTime: %v", err)
			}
			reconciler.clock = clock.NewFakeClock(testTime)

			if test.schedule != nil {
				require.Nil(t, client.Create(ctx, test.schedule))
			}

			_, err = reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "name"}})
			require.Nil(t, err)

			schedule := &velerov1api.Schedule{}
			err = client.Get(ctx, types.NamespacedName{"ns", "name"}, schedule)
			if len(test.expectedPhase) > 0 {
				require.Nil(t, err)
				assert.Equal(t, test.expectedPhase, string(schedule.Status.Phase))
			}
			if len(test.expectedValidationErrors) > 0 {
				require.Nil(t, err)
				assert.EqualValues(t, test.expectedValidationErrors, schedule.Status.ValidationErrors)
			}
			if len(test.expectedLastBackup) > 0 {
				require.Nil(t, err)
				assert.Equal(t, parseTime(test.expectedLastBackup).Unix(), schedule.Status.LastBackup.Unix())
			}

			backups := &velerov1api.BackupList{}
			require.Nil(t, client.List(ctx, backups))
			if test.expectedBackupCreate == nil {
				assert.Equal(t, 0, len(backups.Items))
			} else {
				assert.Equal(t, 1, len(backups.Items))
			}
		})
	}
}

func parseTime(timeString string) time.Time {
	res, _ := time.Parse("2006-01-02 15:04:05", timeString)
	return res
}

func TestGetNextRunTime(t *testing.T) {
	defaultSchedule := func() *velerov1api.Schedule {
		return builder.ForSchedule("velero", "schedule-1").CronSchedule("@every 5m").Result()
	}

	tests := []struct {
		name                      string
		schedule                  *velerov1api.Schedule
		lastRanOffset             string
		expectedDue               bool
		expectedNextRunTimeOffset string
	}{
		{
			name:                      "first run",
			schedule:                  defaultSchedule(),
			expectedDue:               false,
			expectedNextRunTimeOffset: "5m",
		},
		{
			name:                      "just ran",
			schedule:                  defaultSchedule(),
			lastRanOffset:             "0s",
			expectedDue:               false,
			expectedNextRunTimeOffset: "5m",
		},
		{
			name:                      "almost but not quite time to run",
			schedule:                  defaultSchedule(),
			lastRanOffset:             "4m59s",
			expectedDue:               false,
			expectedNextRunTimeOffset: "5m",
		},
		{
			name:                      "time to run again",
			schedule:                  defaultSchedule(),
			lastRanOffset:             "5m",
			expectedDue:               true,
			expectedNextRunTimeOffset: "5m",
		},
		{
			name:                      "several runs missed",
			schedule:                  defaultSchedule(),
			lastRanOffset:             "5h",
			expectedDue:               true,
			expectedNextRunTimeOffset: "5m",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cronSchedule, err := cron.Parse(test.schedule.Spec.Schedule)
			require.NoError(t, err, "unable to parse test.schedule.Spec.Schedule: %v", err)

			testClock := clock.NewFakeClock(time.Now())

			if test.lastRanOffset != "" {
				offsetDuration, err := time.ParseDuration(test.lastRanOffset)
				require.NoError(t, err, "unable to parse test.lastRanOffset: %v", err)

				test.schedule.Status.LastBackup = &metav1.Time{Time: testClock.Now().Add(-offsetDuration)}
				test.schedule.CreationTimestamp = *test.schedule.Status.LastBackup
			} else {
				test.schedule.CreationTimestamp = metav1.Time{Time: testClock.Now()}
			}

			nextRunTimeOffset, err := time.ParseDuration(test.expectedNextRunTimeOffset)
			if err != nil {
				panic(err)
			}

			var baseTime time.Time
			if test.lastRanOffset != "" {
				baseTime = test.schedule.Status.LastBackup.Time
			} else {
				baseTime = test.schedule.CreationTimestamp.Time
			}
			expectedNextRunTime := baseTime.Add(nextRunTimeOffset)

			due, nextRunTime := getNextRunTime(test.schedule, cronSchedule, testClock.Now())

			assert.Equal(t, test.expectedDue, due)
			// ignore diffs of under a second. the cron library does some rounding.
			assert.WithinDuration(t, expectedNextRunTime, nextRunTime, time.Second)
		})
	}
}

func TestParseCronSchedule(t *testing.T) {
	// From https://github.com/vmware-tanzu/velero/issues/30, where we originally were using cron.Parse(),
	// which treats the first field as seconds, and not minutes. We want to use cron.ParseStandard()
	// instead, which has the first field as minutes.

	now := time.Date(2017, 8, 10, 12, 27, 0, 0, time.UTC)

	// Start with a Schedule with:
	// - schedule: once a day at 9am
	// - last backup: 2017-08-10 12:27:00 (just happened)
	s := builder.ForSchedule("velero", "schedule-1").CronSchedule("0 9 * * *").LastBackupTime(now.Format("2006-01-02 15:04:05")).Result()

	logger := velerotest.NewLogger()

	c, errs := parseCronSchedule(s, logger)
	require.Empty(t, errs)

	// make sure we're not due and next backup is tomorrow at 9am
	due, next := getNextRunTime(s, c, now)
	assert.False(t, due)
	assert.Equal(t, time.Date(2017, 8, 11, 9, 0, 0, 0, time.UTC), next)

	// advance the clock a couple of hours and make sure nothing has changed
	now = now.Add(2 * time.Hour)
	due, next = getNextRunTime(s, c, now)
	assert.False(t, due)
	assert.Equal(t, time.Date(2017, 8, 11, 9, 0, 0, 0, time.UTC), next)

	// advance clock to 1 minute after due time, make sure due=true
	now = time.Date(2017, 8, 11, 9, 1, 0, 0, time.UTC)
	due, next = getNextRunTime(s, c, now)
	assert.True(t, due)
	assert.Equal(t, time.Date(2017, 8, 11, 9, 0, 0, 0, time.UTC), next)

	// record backup time
	s.Status.LastBackup = &metav1.Time{Time: now}

	// advance clock 1 minute, make sure we're not due and next backup is tomorrow at 9am
	now = time.Date(2017, 8, 11, 9, 2, 0, 0, time.UTC)
	due, next = getNextRunTime(s, c, now)
	assert.False(t, due)
	assert.Equal(t, time.Date(2017, 8, 12, 9, 0, 0, 0, time.UTC), next)
}

func TestGetBackup(t *testing.T) {
	tests := []struct {
		name           string
		schedule       *velerov1api.Schedule
		testClockTime  string
		expectedBackup *velerov1api.Backup
	}{
		{
			name:           "ensure name is formatted correctly (AM time)",
			schedule:       builder.ForSchedule("foo", "bar").Result(),
			testClockTime:  "2017-07-25 09:15:00",
			expectedBackup: builder.ForBackup("foo", "bar-20170725091500").ObjectMeta(builder.WithLabels(velerov1api.ScheduleNameLabel, "bar")).Result(),
		},
		{
			name:           "ensure name is formatted correctly (PM time)",
			schedule:       builder.ForSchedule("foo", "bar").Result(),
			testClockTime:  "2017-07-25 14:15:00",
			expectedBackup: builder.ForBackup("foo", "bar-20170725141500").ObjectMeta(builder.WithLabels(velerov1api.ScheduleNameLabel, "bar")).Result(),
		},
		{
			name: "ensure schedule backup template is copied",
			schedule: builder.ForSchedule("foo", "bar").
				Template(builder.ForBackup("", "").
					IncludedNamespaces("ns-1", "ns-2").
					ExcludedNamespaces("ns-3").
					IncludedResources("foo", "bar").
					ExcludedResources("baz").
					LabelSelector(&metav1.LabelSelector{MatchLabels: map[string]string{"label": "value"}}).
					TTL(time.Duration(300)).
					Result().
					Spec).
				Result(),
			testClockTime: "2017-07-25 09:15:00",
			expectedBackup: builder.ForBackup("foo", "bar-20170725091500").
				ObjectMeta(builder.WithLabels(velerov1api.ScheduleNameLabel, "bar")).
				IncludedNamespaces("ns-1", "ns-2").
				ExcludedNamespaces("ns-3").
				IncludedResources("foo", "bar").
				ExcludedResources("baz").
				LabelSelector(&metav1.LabelSelector{MatchLabels: map[string]string{"label": "value"}}).
				TTL(time.Duration(300)).
				Result(),
		},
		{
			name:           "ensure schedule labels are copied",
			schedule:       builder.ForSchedule("foo", "bar").ObjectMeta(builder.WithLabels("foo", "bar", "bar", "baz")).Result(),
			testClockTime:  "2017-07-25 14:15:00",
			expectedBackup: builder.ForBackup("foo", "bar-20170725141500").ObjectMeta(builder.WithLabels(velerov1api.ScheduleNameLabel, "bar", "bar", "baz", "foo", "bar")).Result(),
		},
		{
			name:           "ensure schedule annotations are copied",
			schedule:       builder.ForSchedule("foo", "bar").ObjectMeta(builder.WithAnnotations("foo", "bar", "bar", "baz")).Result(),
			testClockTime:  "2017-07-25 14:15:00",
			expectedBackup: builder.ForBackup("foo", "bar-20170725141500").ObjectMeta(builder.WithLabels(velerov1api.ScheduleNameLabel, "bar"), builder.WithAnnotations("bar", "baz", "foo", "bar")).Result(),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			testTime, err := time.Parse("2006-01-02 15:04:05", test.testClockTime)
			require.NoError(t, err, "unable to parse test.testClockTime: %v", err)

			backup := getBackup(test.schedule, clock.NewFakeClock(testTime).Now())

			assert.Equal(t, test.expectedBackup.Namespace, backup.Namespace)
			assert.Equal(t, test.expectedBackup.Name, backup.Name)
			assert.Equal(t, test.expectedBackup.Labels, backup.Labels)
			assert.Equal(t, test.expectedBackup.Annotations, backup.Annotations)
			assert.Equal(t, test.expectedBackup.Spec, backup.Spec)
		})
	}
}
