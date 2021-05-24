package handlers_test

import (
	"net/http"

	"code.cloudfoundry.org/diego-release/rep"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("PingHandler", func() {
	It("responds with 200 OK", func() {
		status, _ := Request(rep.PingRoute, nil, nil)
		Expect(status).To(Equal(http.StatusOK))
	})
})
