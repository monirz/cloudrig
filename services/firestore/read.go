package firestore

import (
	"cloud.google.com/go/firestore/apiv1/firestorepb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// BatchGetDocuments streams the documents a client asked for by reference.
// DocumentRef.Get and Client.GetAll both arrive here; GetDocument never does.
func (s *Service) BatchGetDocuments(req *firestorepb.BatchGetDocumentsRequest, stream firestorepb.Firestore_BatchGetDocumentsServer) error {
	ctx := stream.Context()
	readTime := timestamppb.New(s.clk.Now())

	for _, name := range req.GetDocuments() {
		if err := validDocName(name); err != nil {
			return err
		}

		doc, _, found, err := s.get(ctx, name)
		if err != nil {
			return err
		}

		// A missing document is an ordinary answer, not an error: GetAll
		// reports it per document.
		result := &firestorepb.BatchGetDocumentsResponse{ReadTime: readTime}
		if found {
			result.Result = &firestorepb.BatchGetDocumentsResponse_Found{Found: doc}
		} else {
			result.Result = &firestorepb.BatchGetDocumentsResponse_Missing{Missing: name}
		}
		if err := stream.Send(result); err != nil {
			return err
		}
	}
	return nil
}
