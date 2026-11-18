//! Person deletion markers (`spec/manifest.md` §9).

use crate::error::Result;
use crate::identity::IdentityMap;
use crate::{keys, Store};
use serde::{Deserialize, Serialize};
use std::collections::HashSet;

#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
pub struct Tombstone {
    #[serde(default)]
    pub id: String,
    pub ts_ms: i64,
    #[serde(default)]
    pub person_keys: Vec<String>,
    #[serde(default)]
    pub anon_ids: Vec<String>,
}

pub fn tombstone_key(project: u32, t: &Tombstone) -> String {
    format!("{}/{:013}-{}.json", keys::tombstone_prefix(project), t.ts_ms.max(0), t.id)
}

/// All tombstones of a project, oldest first. Unparseable files are skipped
/// with a warning rather than blocking queries.
pub async fn load_all(store: &Store, project: u32) -> Result<Vec<Tombstone>> {
    let mut out = Vec::new();
    for o in store.list(&keys::tombstone_prefix(project)).await? {
        if !o.key.ends_with(".json") {
            continue;
        }
        match store.get_json::<Tombstone>(&o.key).await {
            Ok(Some(t)) => out.push(t),
            Ok(None) => {}
            Err(e) => tracing::warn!(key = %o.key, error = %e, "skipping unreadable tombstone"),
        }
    }
    out.sort_by(|a, b| a.ts_ms.cmp(&b.ts_ms).then_with(|| a.id.cmp(&b.id)));
    Ok(out)
}

/// Expanded row filter built from the tombstones newer than `after_ts`.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct TombstoneSet {
    pub person_keys: HashSet<String>,
    pub anon_ids: HashSet<String>,
    /// Largest `ts_ms` included (0 if empty).
    pub max_ts: i64,
}

impl TombstoneSet {
    /// Includes tombstones with `ts_ms > after_ts`. Each `u:U` is expanded to
    /// every anonymous id the identity map links to `U`.
    pub fn build(tombstones: &[Tombstone], identity: &IdentityMap, after_ts: i64) -> Self {
        let mut set = Self::default();
        let mut users: HashSet<String> = HashSet::new();
        for t in tombstones.iter().filter(|t| t.ts_ms > after_ts) {
            set.max_ts = set.max_ts.max(t.ts_ms);
            for pk in &t.person_keys {
                set.person_keys.insert(pk.clone());
                if let Some(u) = pk.strip_prefix("u:") {
                    users.insert(u.to_owned());
                } else if let Some(a) = pk.strip_prefix("a:") {
                    set.anon_ids.insert(a.to_owned());
                }
            }
            set.anon_ids.extend(t.anon_ids.iter().cloned());
        }
        if !users.is_empty() {
            set.anon_ids.extend(identity.anons_of(&users));
        }
        // An anonymous id that is deleted may already be resolved to a user in
        // newer files; rows are still caught through their `anon_id` column.
        for a in &set.anon_ids {
            set.person_keys.insert(format!("a:{a}"));
        }
        set
    }

    pub fn is_empty(&self) -> bool {
        self.person_keys.is_empty() && self.anon_ids.is_empty()
    }

    /// Whether a row belongs to a deleted person.
    pub fn matches(&self, person_key: &str, anon_id: &str, user_id: Option<&str>) -> bool {
        if self.is_empty() {
            return false;
        }
        self.person_keys.contains(person_key)
            || self.anon_ids.contains(anon_id)
            || user_id.is_some_and(|u| !u.is_empty() && self.person_keys.contains(&format!("u:{u}")))
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::identity::{append_delta, Candidate};

    #[tokio::test]
    async fn load_sorts_and_skips_garbage() {
        let s = Store::memory();
        let a = Tombstone { id: "del_b".into(), ts_ms: 2000, person_keys: vec!["u:2".into()], anon_ids: vec![] };
        let b = Tombstone { id: "del_a".into(), ts_ms: 1000, person_keys: vec![], anon_ids: vec!["x".into()] };
        s.put_json(&tombstone_key(7, &a), &a).await.unwrap();
        s.put_json(&tombstone_key(7, &b), &b).await.unwrap();
        s.put("tombstones/p=7/0000000003000-bad.json", &b"{not json"[..]).await.unwrap();
        assert_eq!(tombstone_key(7, &b), "tombstones/p=7/0000000001000-del_a.json");
        assert_eq!(load_all(&s, 7).await.unwrap(), [b, a]);
        assert!(load_all(&s, 8).await.unwrap().is_empty());
    }

    #[tokio::test]
    async fn expands_users_to_their_anonymous_ids() {
        let s = Store::memory();
        let mut id = IdentityMap::default();
        append_delta(
            &s,
            1,
            "1-0",
            1,
            vec![
                Candidate { anon_id: "dev1".into(), user_id: "42".into(), ts: 1, uuid: [1; 16] },
                Candidate { anon_id: "dev2".into(), user_id: "42".into(), ts: 2, uuid: [2; 16] },
                Candidate { anon_id: "dev3".into(), user_id: "7".into(), ts: 3, uuid: [3; 16] },
            ],
            &mut id,
        )
        .await
        .unwrap();
        let tombs = vec![
            Tombstone { id: "old".into(), ts_ms: 100, person_keys: vec!["u:7".into()], anon_ids: vec![] },
            Tombstone {
                id: "new".into(),
                ts_ms: 200,
                person_keys: vec!["u:42".into()],
                anon_ids: vec!["loose".into()],
            },
        ];
        let set = TombstoneSet::build(&tombs, &id, 100);
        assert_eq!(set.max_ts, 200);
        assert!(set.matches("u:42", "whatever", Some("42")));
        assert!(set.matches("a:dev1", "dev1", None), "pre-identify rows of the deleted user");
        assert!(set.matches("u:other", "dev2", Some("other")), "caught by anon_id");
        assert!(set.matches("u:shared", "x", Some("42")), "caught by user_id column");
        assert!(set.matches("a:loose", "loose", None));
        assert!(!set.matches("u:7", "dev3", Some("7")), "older tombstone excluded by after_ts");
        assert!(!set.matches("u:1", "a", None));
        let all = TombstoneSet::build(&tombs, &id, 0);
        assert!(all.matches("u:7", "dev3", Some("7")));
        assert!(TombstoneSet::build(&tombs, &id, 200).is_empty());
    }
}
