// Nostr library event model.
class Event {
  String id;
  String pubkey;
  int created_at;
  int kind;
  List<List<String>> tags;
  String content;
  String sig;
}
