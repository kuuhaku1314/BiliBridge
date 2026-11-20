// BiliBridge local album adapter for PotPlayer's Media PlayParse interface.
string GetTitle() { return "BiliBridge"; }
string GetVersion() { return "1.3"; }
string GetDesc() { return "BiliBridge - B站首页、我的收藏和我关注的直播"; }

bool IsLocal(const string &in path) {
    return path.find("http://127.0.0.1:17863/") == 0;
}

bool PlaylistCheck(const string &in path) {
    return IsLocal(path) && (path.find("/home.bbalbum") >= 0 || path.find("/favorites.bbalbum") >= 0 || path.find("/live.bbalbum") >= 0);
}

bool PlayitemCheck(const string &in path) {
    return IsLocal(path) && path.find("/item/") >= 0 &&
        (path.find(".bbvideo") >= 0 || path.find(".bblive") >= 0);
}

array<dictionary> PlaylistParse(const string &in path) {
    array<dictionary> videos;
    if (!PlaylistCheck(path)) return videos;
    string data = HostUrlGetStringWithAPI(path, "BiliBridge", "", "", true);
    JsonReader reader;
    JsonValue root;
    if (!reader.parse(data, root) || !root.isArray()) return videos;
    for (int i = 0; i < root.size(); i++) {
        JsonValue entry = root[i];
        string url = entry["url"].asString();
        if (!PlayitemCheck(url)) continue;
        dictionary video;
        video["url"] = url;
        video["title"] = entry["title"].asString();
        video["author"] = entry["author"].asString();
        video["thumbnail"] = entry["thumbnail"].asString();
        video["duration"] = entry["duration"].asInt();
        videos.insertLast(video);
    }
    return videos;
}

string PlayitemParse(const string &in path, dictionary &MetaData, array<dictionary> &QualityList) {
    if (!PlayitemCheck(path)) return "";
    string data = HostUrlGetStringWithAPI(path + "?tracks=1", "BiliBridge", "", "", true);
    JsonReader reader;
    JsonValue root;
    if (!reader.parse(data, root) || !root.isObject()) return "";
    string title = root["title"].asString();
    if (!title.empty()) MetaData["title"] = title;
    string thumbnail = root["thumbnail"].asString();
    if (!thumbnail.empty()) MetaData["thumbnail"] = thumbnail;
    return root["url"].asString();
}
