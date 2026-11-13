package phone

import "testing"

func TestMediaStaysWhileAnotherCallUsesIt(t *testing.T) {
	s := &Service{calls: map[string]*activeCall{
		"outbound": {mediaID: "media-1", view: CallView{CallID: "outbound"}},
		"inbound":  {mediaID: "media-1", view: CallView{CallID: "inbound"}},
	}}
	if !s.mediaSharedByLiveCall("media-1", "outbound") {
		t.Fatal("shared media was treated as unused")
	}
	if s.mediaSharedByLiveCall("media-1", "inbound") != true {
		t.Fatal("second call lost sight of the shared media")
	}
	if s.mediaSharedByLiveCall("media-2", "outbound") {
		t.Fatal("unrelated media reported as shared")
	}
}

func TestMediaReleasedAfterTheLastCallEnds(t *testing.T) {
	s := &Service{calls: map[string]*activeCall{
		"outbound": {mediaID: "media-1", view: CallView{CallID: "outbound"}, terminal: true},
		"inbound":  {mediaID: "media-1", view: CallView{CallID: "inbound"}},
	}}
	if s.mediaSharedByLiveCall("media-1", "inbound") {
		t.Fatal("ended call still held the media")
	}
	if s.mediaSharedByLiveCall("", "inbound") {
		t.Fatal("empty media id reported as shared")
	}
}
