package events

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
	"sync"

	"github.com/thetorproject/proteus/proteus-common/middleware"
	"github.com/apex/log"
	"github.com/satori/go.uuid"
	"github.com/lib/pq"
	"github.com/jmoiron/sqlx"
	"github.com/jmoiron/sqlx/types"
	"github.com/spf13/viper"
	"github.com/gin-gonic/gin"
	"github.com/facebookgo/grace/gracehttp"
	"gopkg.in/gin-contrib/cors.v1"
)

var ctx = log.WithFields(log.Fields{
	"cmd": "events",
})

func initDatabase() (*sqlx.DB, error) {
	db, err := sqlx.Open("postgres", viper.GetString("database.url"))
	if err != nil {
		ctx.Error("failed to open database")
		return nil, err
	}
	return db, err
}

type Target struct {
	Countries	[]string `json:"countries"`
	Platforms	[]string `json:"platforms"`
}

type URLTestArg struct {
	GlobalCategories	[]string `json:"global_categories"`
	CountryCategories	[]string `json:"country_categories"`
	URLs				[]string `json:"urls"`
}

type Task struct {
	Id			string `json:"id"`
	TestName	string `json:"test_name" binding:"required"`
	Arguments	interface{} `json:"arguments"`
	State		string
}

type JobData struct {
	Schedule		string `json:"schedule" binding:"required"`
	Delay			int64 `json:"delay"`
	Comment			string `json:"comment" binding:"required"`
	Task			Task `json:"task"`
	Target			Target `json:"target"`

	CreationTime	time.Time `json:"creation_time"`
}

func AddJob(db *sqlx.DB, jd JobData, s *Scheduler) (string, error) {
	schedule, err := ParseSchedule(jd.Schedule)
	if err != nil {
		ctx.WithError(err).Error("invalid schedule format")
		return "", err
	}

	tx, err := db.Begin()
	if err != nil {
		ctx.WithError(err).Error("failed to open transaction")
		return "", err
	}

	var jobID = uuid.NewV4().String()
	{
		query := fmt.Sprintf(`INSERT INTO %s (
			id, comment,
			schedule, delay,
			target_countries,
			target_platforms,
			task_test_name,
			task_arguments,
			creation_time,
			times_run,
			next_run_at,
			is_done
		) VALUES (
			$1, $2,
			$3, $4,
			$5,
			$6,
			$7,
			$8,
			$9,
			$10,
			$11,
			$12)`,
		pq.QuoteIdentifier(viper.GetString("database.jobs-table")))

		stmt, err := tx.Prepare(query)
		if (err != nil) {
			ctx.WithError(err).Error("failed to prepare jobs query")
			return "", err
		}
		defer stmt.Close()

		taskArgsStr, err := json.Marshal(jd.Task.Arguments)
		if err != nil {
			ctx.WithError(err).Error("failed to serialise task arguments")
			return "", err
		}
		_, err = stmt.Exec(jobID, jd.Comment,
							jd.Schedule, jd.Delay,
							pq.Array(jd.Target.Countries),
							pq.Array(jd.Target.Platforms),
							jd.Task.TestName,
							taskArgsStr,
							time.Now().UTC(),
							0,
							schedule.StartTime,
							false)
		if err != nil {
			tx.Rollback()
			ctx.WithError(err).Error("failed to insert into jobs table")
			return "", err
		}
	}

	if err = tx.Commit(); err != nil {
		ctx.WithError(err).Error("failed to commit transaction, rolling back")
		return "", err
	}
	j := Job{
		Id: jobID,
		Comment: jd.Comment,
		Schedule: schedule,
		Delay: jd.Delay,
		TimesRun: 0,
		lock: sync.RWMutex{},
		IsDone: false,
		NextRunAt: schedule.StartTime,
	}
	go s.RunJob(&j)

	return jobID, nil
}

func ListJobs(db *sqlx.DB) ([]JobData, error) {
	// XXX this can probably be unified with JobDB.GetAll()
	var (
		currentJobs []JobData
	)
	query := fmt.Sprintf(`SELECT
		id, comment,
		schedule, delay,
		target_countries,
		target_platforms,
		task_test_name,
		task_arguments
		FROM %s`,
		pq.QuoteIdentifier(viper.GetString("database.jobs-table")))
	rows, err := db.Query(query)
	if err != nil {
		ctx.WithError(err).Error("failed to list jobs")
		return currentJobs, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			jd JobData
			taskArgs types.JSONText
		)
		err := rows.Scan(&jd.Comment,
						&jd.Schedule,
						&jd.Delay,
						&jd.Target.Countries,
						&jd.Target.Platforms,
						&jd.Task.TestName,
						&taskArgs)
		if err != nil {
			ctx.WithError(err).Error("failed to iterate over jobs")
			return currentJobs, err
		}
		err = taskArgs.Unmarshal(&jd.Task.Arguments)
		if err != nil {
			ctx.WithError(err).Error("failed to unmarshal JSON")
			return currentJobs, err
		}
		currentJobs = append(currentJobs, jd)
	}
	return currentJobs, nil
}

var ErrTaskNotFound = errors.New("task not found")
var ErrAccessDenied = errors.New("access denied")
var ErrInconsistentState = errors.New("task already accepted")

func GetTask(tID string, uID string, db *sqlx.DB) (Task, error) {
	var (
		err error
		probeId string
		taskArgs types.JSONText
	)
	task := Task{}
	query := fmt.Sprintf(`SELECT
		id,
		probe_id,
		test_name,
		arguments,
		state
		FROM %s
		WHERE id = $1`,
		pq.QuoteIdentifier(viper.GetString("database.tasks-table")))
	err = db.QueryRow(query, tID).Scan(
		&task.Id,
		&probeId,
		&task.TestName,
		&taskArgs,
		&task.State)
	if err != nil {
		if err == sql.ErrNoRows {
			return task, ErrTaskNotFound
		}
		ctx.WithError(err).Error("failed to get task")
		return task, err
	}
	if probeId != uID {
		return task, ErrAccessDenied
	}
	err = taskArgs.Unmarshal(&task.Arguments)
	if err != nil {
		ctx.WithError(err).Error("failed to unmarshal json")
		return task, err
	}
	return task, nil
}

func GetTasksForUser(uID string, since string,
						db *sqlx.DB) ([]Task, error) {
	var (
		err error
		tasks []Task
	)
	query := fmt.Sprintf(`SELECT
		id,
		test_name,
		arguments
		FROM %s
		WHERE
		state = 'ready' AND
		probe_id = $1 AND creation_time >= $2`,
		pq.QuoteIdentifier(viper.GetString("database.tasks-table")))

	rows, err := db.Query(query, uID, since)
	if err != nil {
		if err == sql.ErrNoRows {
			return tasks, nil
		}
		ctx.WithError(err).Error("failed to get task list")
		return tasks, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			taskArgs types.JSONText
			task Task
		)
		rows.Scan(&task.Id, &task.TestName, &taskArgs)
		if err != nil {
			ctx.WithError(err).Error("failed to get task")
			return tasks, err
		}
		err = taskArgs.Unmarshal(&task.Arguments)
		if err != nil {
			ctx.WithError(err).Error("failed to unmarshal json")
			return tasks, err
		}
		tasks = append(tasks, task)
	}
	return tasks, nil
}

func SetTaskState(tID string, uID string,
					state string, validStates []string,
					updateTimeCol string,
					db *sqlx.DB) (error) {
	var err error
	task, err := GetTask(tID, uID, db)
	if err != nil {
		return err
	}
	stateConsistent := false
	for _, s := range validStates {
		if task.State == s {
			stateConsistent = true
			break
		}
	}
	if !stateConsistent {
		return ErrInconsistentState
	}

	query := fmt.Sprintf(`UPDATE %s SET
		state = $2,
		%s = $3,
		last_updated = $3
		WHERE id = $1`,
		pq.QuoteIdentifier(viper.GetString("database.tasks-table")),
		updateTimeCol)

	_, err = db.Exec(query, tID, state, time.Now().UTC())
	if err != nil {
		ctx.WithError(err).Error("failed to get task")
		return err
	}
	return nil
}

func Start() {
	db, err := initDatabase()

	if (err != nil) {
		ctx.WithError(err).Error("failed to connect to DB")
		return
	}
	defer db.Close()

	authMiddleware, err := jwt.InitAuthMiddleware(db)
	if (err != nil) {
		ctx.WithError(err).Error("failed to initialise authMiddlewareDevice")
		return
	}

	scheduler := NewScheduler(db)

	corsConfig := cors.DefaultConfig()
	corsConfig.AllowAllOrigins = true

	router := gin.Default()
	router.Use(cors.New(corsConfig))
	v1 := router.Group("/api/v1")

	admin := v1.Group("/admin")
	// XXX CRITICAL temporarily disabled for debug
	//admin.Use(authMiddleware.MiddlewareFunc(jwt.AdminAuthorizor))
	{
		admin.GET("/jobs", func(c *gin.Context) {
			// XXX do this in a middleware
			c.Writer.Header().Set("Access-Control-Allow-Origin", "*")
			jobList, err := ListJobs(db)
			if err != nil {
				c.JSON(http.StatusBadRequest,
						gin.H{"error": err.Error()})
				return
			}
			c.JSON(http.StatusOK,
					gin.H{"jobs": jobList})
		})
		admin.POST("/job", func(c *gin.Context) {
			c.Writer.Header().Set("Access-Control-Allow-Origin", "*")
			var jobData JobData
			err := c.BindJSON(&jobData)
			if err != nil {
				ctx.WithError(err).Error("invalid request")
				c.JSON(http.StatusBadRequest,
						gin.H{"error": "invalid request"})
				return
			}
			jobID, err := AddJob(db, jobData, scheduler)
			if (err != nil) {
				c.JSON(http.StatusBadRequest,
						gin.H{"error": err.Error()})
				return
			}

			c.JSON(http.StatusOK,
					gin.H{"id": jobID})
			return
		})
	}

	device := v1.Group("/")
	device.Use(authMiddleware.MiddlewareFunc(jwt.DeviceAuthorizor))
	{
		device.GET("/tasks", func(c *gin.Context) {
			userId := c.MustGet("userID").(string)
			since := c.DefaultQuery("since", "2016-10-20T10:30:00Z")
			_, err := time.Parse(ISOUTCTimeLayout, since)
			if err != nil {
				c.JSON(http.StatusBadRequest,
						gin.H{"error": "invalid since specified"})
				return
			}
			tasks, err := GetTasksForUser(userId, since, db)
			if err != nil {
				c.JSON(http.StatusInternalServerError,
						gin.H{"error": "server side error"})
				return
			}
			c.JSON(http.StatusOK,
					gin.H{"tasks": tasks})
		})

		device.GET("/task/:task_id", func(c *gin.Context) {
			taskID := c.Param("task_id")
			userId := c.MustGet("userID").(string)
			task, err := GetTask(taskID, userId, db)
			if err != nil {
				if err == ErrAccessDenied {
					c.JSON(http.StatusUnauthorized,
							gin.H{"error": "access denied"})
					return
				}
				if err == ErrTaskNotFound {
					// XXX is it a concern that a user this way can enumerate
					// tasks of other users?
					// I don't think it's a security issue, but it's worth
					// thinking about...
					c.JSON(http.StatusNotFound,
							gin.H{"error": "task not found"})
					return
				}
				c.JSON(http.StatusBadRequest,
						gin.H{"error": "invalid request"})
				return
			}
			c.JSON(http.StatusOK,
					gin.H{"id": task.Id,
						"test_name": task.TestName,
						"arguments": task.Arguments})
			return
		})
		device.POST("/task/:task_id/accept", func(c *gin.Context) {
			taskID := c.Param("task_id")
			userId := c.MustGet("userID").(string)
			err := SetTaskState(taskID,
								userId,
								"accepted",
								[]string{"ready", "notified"},
								"accept_time",
								db)
			if err != nil {
				if err == ErrInconsistentState {
					c.JSON(http.StatusBadRequest,
							gin.H{"error": "task already accepted"})
					return
				}
				if err == ErrAccessDenied {
					c.JSON(http.StatusUnauthorized,
							gin.H{"error": "access denied"})
					return
				}
				if err == ErrTaskNotFound {
					c.JSON(http.StatusNotFound,
							gin.H{"error": "task not found"})
					return
				}
			}
			c.JSON(http.StatusOK,
					gin.H{"status": "accepted"})
			return
		})
		device.POST("/task/:task_id/reject", func(c *gin.Context) {
			taskID := c.Param("task_id")
			userId := c.MustGet("userID").(string)
			err := SetTaskState(taskID,
								userId,
								"rejected",
								[]string{"ready", "notified", "accepted"},
								"done_time",
								db)
			if err != nil {
				if err == ErrInconsistentState {
					c.JSON(http.StatusBadRequest,
							gin.H{"error": "task already done"})
					return
				}
				if err == ErrAccessDenied {
					c.JSON(http.StatusUnauthorized,
							gin.H{"error": "access denied"})
					return
				}
				if err == ErrTaskNotFound {
					c.JSON(http.StatusNotFound,
							gin.H{"error": "task not found"})
					return
				}
			}
			c.JSON(http.StatusOK,
					gin.H{"status": "rejected"})
			return
		})
		device.POST("/task/:task_id/done", func(c *gin.Context) {
			taskID := c.Param("task_id")
			userId := c.MustGet("userID").(string)
			err := SetTaskState(taskID,
								userId,
								"done",
								[]string{"accepted"},
								"done_time",
								db)
			if err != nil {
				if err == ErrInconsistentState {
					c.JSON(http.StatusBadRequest,
							gin.H{"error": "task already done"})
					return
				}
				if err == ErrAccessDenied {
					c.JSON(http.StatusUnauthorized,
							gin.H{"error": "access denied"})
					return
				}
				if err == ErrTaskNotFound {
					c.JSON(http.StatusNotFound,
							gin.H{"error": "task not found"})
					return
				}
			}
			c.JSON(http.StatusOK,
					gin.H{"status": "done"})
			return
		})
	}

	Addr := fmt.Sprintf("%s:%d", viper.GetString("api.address"),
								viper.GetInt("api.port"))
	ctx.Infof("starting on %s", Addr)

	scheduler.Start()
	s := &http.Server{
		Addr: Addr,
		Handler: router,
	}
	gracehttp.Serve(s)
}
