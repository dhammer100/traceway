package middleware

import (
	"github.com/tracewayapp/traceway/backend/app/db"
	"database/sql"
	"net/http"

	"github.com/gin-gonic/gin"
)

const TransactionContextKey = "dbTx"
const postCommitHooksKey = "dbTxPostCommit"

func Transactional(c *gin.Context) {
	txHandle, err := db.DB.Begin()

	if err != nil {
		c.AbortWithStatus(http.StatusInternalServerError)
		panic(err)
	}

	defer func() {
		if r := recover(); r != nil {
			txHandle.Rollback()
			c.AbortWithStatus(http.StatusInternalServerError)
			panic(r)
		}
	}()

	c.Set(TransactionContextKey, txHandle)

	c.Next()

	if status := c.Writer.Status(); status == http.StatusOK || status == http.StatusCreated || status == http.StatusSeeOther {
		if err := txHandle.Commit(); err != nil {
			c.AbortWithStatus(http.StatusInternalServerError)
			panic(err)
		}
		// Only run post-commit hooks once the DB has durably accepted the
		// changes — e.g. cache mutations that would otherwise outlive a
		// rolled-back insert.
		if raw, ok := c.Get(postCommitHooksKey); ok {
			if hooks, ok := raw.([]func()); ok {
				for _, h := range hooks {
					h()
				}
			}
		}
	} else {
		txHandle.Rollback()
	}
}

func GetTx(c *gin.Context) *sql.Tx {
	if id, exists := c.Get(TransactionContextKey); exists {
		return id.(*sql.Tx)
	}
	return nil
}

// AfterCommit registers a function to run after the transaction associated
// with this request commits. Use for side effects that must not happen if the
// DB write is rolled back (e.g. in-memory cache updates). If there is no
// transactional context, the function runs immediately.
func AfterCommit(c *gin.Context, fn func()) {
	raw, ok := c.Get(postCommitHooksKey)
	if !ok {
		// No tx — caller is outside Transactional middleware; running inline
		// is the only reasonable option.
		if _, hasTx := c.Get(TransactionContextKey); !hasTx {
			fn()
			return
		}
		c.Set(postCommitHooksKey, []func(){fn})
		return
	}
	hooks, _ := raw.([]func())
	c.Set(postCommitHooksKey, append(hooks, fn))
}
