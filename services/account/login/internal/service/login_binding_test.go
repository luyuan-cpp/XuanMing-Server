package service

import (
	"context"
	"testing"

	"github.com/luyuancpp/pandora/pkg/auth"
	commonv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/common/v1"
	loginv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/login/v1"
	"github.com/luyuancpp/pandora/services/account/login/internal/biz"
	"github.com/luyuancpp/pandora/services/account/login/internal/data"
)

type acceptingAssignmentChecker struct{}

func (acceptingAssignmentChecker) CheckCurrent(context.Context, uint64, data.HubAssignmentBinding) error {
	return nil
}

func TestVerifyDSTicketResponsePassesThroughHubBinding(t *testing.T) {
	cfg := auth.Config{Secret: []byte("pandora-test-only-secret-32-bytes!!")}
	signer, err := auth.NewSigner(cfg)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := auth.NewVerifier(cfg)
	if err != nil {
		t.Fatal(err)
	}
	binding := auth.DSTicketBinding{
		DSPodName:       "hub-cn-1",
		DSInstanceUID:   "uid-a",
		ProtocolEpoch:   7,
		CredentialGen:   42,
		CredentialJTI:   "credential-jti-a",
		HubAssignmentID: "assignment-a",
		WriterEpoch:     2,
	}
	ticket, _, err := signer.SignBoundHubDSTicket(1001, 3, 33, 9, "entry-jti-a", binding)
	if err != nil {
		t.Fatal(err)
	}
	ticketUC := biz.NewTicketUsecase(signer, verifier, nil)
	ticketUC.SetHubAssignmentBindingPolicy(true, acceptingAssignmentChecker{})
	svc := NewLoginService(nil, ticketUC)

	res, err := svc.VerifyDSTicket(context.Background(), &loginv1.VerifyDSTicketRequest{
		Ticket:    ticket,
		DsPodName: binding.DSPodName,
	})
	if err != nil {
		t.Fatalf("VerifyDSTicket transport error: %v", err)
	}
	if res.GetCode() != commonv1.ErrCode_OK {
		t.Fatalf("code=%v", res.GetCode())
	}
	claims := res.GetClaims()
	if claims.GetDsPodName() != binding.DSPodName ||
		claims.GetDsInstanceUid() != binding.DSInstanceUID ||
		claims.GetDsProtocolEpoch() != binding.ProtocolEpoch ||
		claims.GetDsCredentialGen() != binding.CredentialGen ||
		claims.GetDsCredentialJti() != binding.CredentialJTI ||
		claims.GetHubAssignmentId() != binding.HubAssignmentID ||
		claims.GetDsWriterEpoch() != binding.WriterEpoch {
		t.Fatalf("response binding mismatch: %+v", claims)
	}
}
