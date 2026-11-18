//! Breakdown-value interning without allocating on the lookup path.

use std::collections::HashMap;

#[derive(Debug, Default, Clone)]
pub struct Interner {
    ids: HashMap<String, u32>,
    null_id: Option<u32>,
    names: Vec<Option<String>>,
}

impl Interner {
    /// Id of `value`, and whether it was just created.
    pub fn intern(&mut self, value: Option<&str>) -> (u32, bool) {
        let existing = match value {
            Some(v) => self.ids.get(v).copied(),
            None => self.null_id,
        };
        if let Some(id) = existing {
            return (id, false);
        }
        let id = self.names.len() as u32;
        match value {
            Some(v) => {
                self.ids.insert(v.to_owned(), id);
            }
            None => self.null_id = Some(id),
        }
        self.names.push(value.map(str::to_owned));
        (id, true)
    }

    pub fn len(&self) -> usize {
        self.names.len()
    }

    pub fn is_empty(&self) -> bool {
        self.names.is_empty()
    }

    pub fn name(&self, id: u32) -> &Option<String> {
        &self.names[id as usize]
    }

    pub fn into_names(self) -> Vec<Option<String>> {
        self.names
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn interns_null_and_strings_separately() {
        let mut i = Interner::default();
        assert_eq!(i.intern(Some("a")), (0, true));
        assert_eq!(i.intern(None), (1, true));
        assert_eq!(i.intern(Some("")), (2, true));
        assert_eq!(i.intern(Some("a")), (0, false));
        assert_eq!(i.intern(None), (1, false));
        assert_eq!(i.into_names(), [Some("a".to_owned()), None, Some(String::new())]);
    }
}
