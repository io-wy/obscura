use std::net::Ipv4Addr;
use std::process::{Command, Stdio};
use thiserror::Error;
use uuid::Uuid;

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct NetworkSpec {
    pub sandbox_id: Uuid,
    pub tap_device: String,
    pub host_ipv4: Ipv4Addr,
    pub guest_ipv4: Ipv4Addr,
    pub prefix_len: u8,
    pub uplink: String,
    pub allow_egress: bool,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct NetworkHandle {
    pub tap_device: String,
    pub table_name: String,
    pub guest_ipv4: Ipv4Addr,
}

pub fn addresses_for_slot(slot: u32) -> Result<(Ipv4Addr, Ipv4Addr), NetworkError> {
    if slot == 0 || slot > 16_383 {
        return Err(NetworkError::InvalidSpec(
            "IPv4 /30 pool is exhausted".into(),
        ));
    }
    let base = u32::from(Ipv4Addr::new(172, 30, 0, 0)) + slot * 4;
    Ok((Ipv4Addr::from(base + 1), Ipv4Addr::from(base + 2)))
}

#[derive(Debug, Error)]
pub enum NetworkError {
    #[error("invalid network specification: {0}")]
    InvalidSpec(String),
    #[error("command {program} failed: {stderr}")]
    CommandFailed { program: String, stderr: String },
    #[error("I/O error: {0}")]
    Io(#[source] std::io::Error),
}

pub trait CommandRunner {
    fn run(
        &mut self,
        program: &str,
        args: &[String],
        stdin: Option<&[u8]>,
    ) -> Result<(), NetworkError>;
}

pub struct SystemCommandRunner;

impl CommandRunner for SystemCommandRunner {
    fn run(
        &mut self,
        program: &str,
        args: &[String],
        stdin: Option<&[u8]>,
    ) -> Result<(), NetworkError> {
        let mut command = Command::new(program);
        command
            .args(args)
            .stdout(Stdio::null())
            .stderr(Stdio::piped());
        if stdin.is_some() {
            command.stdin(Stdio::piped());
        }
        let mut child = command.spawn().map_err(NetworkError::Io)?;
        if let Some(input) = stdin {
            use std::io::Write;
            child
                .stdin
                .take()
                .expect("stdin was configured")
                .write_all(input)
                .map_err(NetworkError::Io)?;
        }
        let output = child.wait_with_output().map_err(NetworkError::Io)?;
        if !output.status.success() {
            return Err(NetworkError::CommandFailed {
                program: program.into(),
                stderr: String::from_utf8_lossy(&output.stderr).trim().into(),
            });
        }
        Ok(())
    }
}

pub struct NetworkManager<R> {
    runner: R,
}

impl<R: CommandRunner> NetworkManager<R> {
    pub fn new(runner: R) -> Self {
        Self { runner }
    }

    pub fn provision(&mut self, spec: &NetworkSpec) -> Result<NetworkHandle, NetworkError> {
        validate(spec)?;
        let table_name = table_name(spec.sandbox_id);
        let prefix = spec.prefix_len.to_string();
        let host_address = format!("{}/{}", spec.host_ipv4, prefix);
        let tap_args = vec![
            "tuntap".into(),
            "add".into(),
            "dev".into(),
            spec.tap_device.clone(),
            "mode".into(),
            "tap".into(),
        ];
        self.runner.run("ip", &tap_args, None)?;
        let address_args = vec![
            "addr".into(),
            "add".into(),
            host_address,
            "dev".into(),
            spec.tap_device.clone(),
        ];
        if let Err(error) = self.runner.run("ip", &address_args, None) {
            let _ = self.cleanup_tap(&spec.tap_device);
            return Err(error);
        }
        let link_args = vec![
            "link".into(),
            "set".into(),
            "dev".into(),
            spec.tap_device.clone(),
            "up".into(),
        ];
        if let Err(error) = self.runner.run("ip", &link_args, None) {
            let _ = self.cleanup_tap(&spec.tap_device);
            return Err(error);
        }
        let rules = nft_rules(&table_name, spec);
        if let Err(error) =
            self.runner
                .run("nft", &["-f".into(), "-".into()], Some(rules.as_bytes()))
        {
            let _ = self.cleanup_rules(&table_name);
            let _ = self.cleanup_tap(&spec.tap_device);
            return Err(error);
        }
        Ok(NetworkHandle {
            tap_device: spec.tap_device.clone(),
            table_name,
            guest_ipv4: spec.guest_ipv4,
        })
    }

    pub fn cleanup(&mut self, handle: &NetworkHandle) -> Result<(), NetworkError> {
        let rules_result = self.cleanup_rules(&handle.table_name);
        let tap_result = self.cleanup_tap(&handle.tap_device);
        rules_result.and(tap_result)
    }

    fn cleanup_rules(&mut self, table_name: &str) -> Result<(), NetworkError> {
        let mut first_error = None;
        for family in ["inet", "ip"] {
            let rule = format!("delete table {family} {table_name}\n");
            if let Err(error) =
                self.runner
                    .run("nft", &["-f".into(), "-".into()], Some(rule.as_bytes()))
            {
                if !already_absent(&error) {
                    first_error.get_or_insert(error);
                }
            }
        }
        first_error.map_or(Ok(()), Err)
    }

    /// The VM must already be stopped and its TAP removed. Scope deletion to
    /// this guest; flushing the host table would disrupt unrelated tenants.
    pub fn clear_connections(&mut self, guest: Ipv4Addr) -> Result<(), NetworkError> {
        for selector in ["--orig-src", "--orig-dst"] {
            let result = self.runner.run(
                "conntrack",
                &[
                    "--delete".into(),
                    "--family".into(),
                    "ipv4".into(),
                    selector.into(),
                    guest.to_string(),
                ],
                None,
            );
            match result {
                // conntrack-tools returns exit status 1 when no entries match.
                Err(NetworkError::CommandFailed { program, stderr })
                    if program == "conntrack"
                        && stderr.lines().count() == 1
                        && stderr.contains(": 0 flow entries have been deleted.") => {}
                result => result?,
            }
        }
        Ok(())
    }

    fn cleanup_tap(&mut self, tap_device: &str) -> Result<(), NetworkError> {
        let args = vec![
            "link".into(),
            "delete".into(),
            "dev".into(),
            tap_device.into(),
        ];
        match self.runner.run("ip", &args, None) {
            Err(error) if already_absent(&error) => Ok(()),
            result => result,
        }
    }
}

// Only absence is success. Permission errors, command launch failures and all other
// cleanup failures must retain the durable resource claim for a subsequent retry.
fn already_absent(error: &NetworkError) -> bool {
    matches!(error, NetworkError::CommandFailed { program, stderr }
        if (program == "ip" && stderr.starts_with("Cannot find device "))
        || (program == "nft" && stderr.contains("Could not process rule: No such file or directory")))
}

fn validate(spec: &NetworkSpec) -> Result<(), NetworkError> {
    if !valid_name(&spec.tap_device) || !valid_name(&spec.uplink) {
        return Err(NetworkError::InvalidSpec(
            "interface names contain unsupported characters".into(),
        ));
    }
    if spec.tap_device == spec.uplink {
        return Err(NetworkError::InvalidSpec(
            "tap and uplink must differ".into(),
        ));
    }
    if !(1..=31).contains(&spec.prefix_len) {
        return Err(NetworkError::InvalidSpec(
            "prefix length must be between 1 and 31".into(),
        ));
    }
    Ok(())
}

fn valid_name(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= 15
        && value
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || b"_.-".contains(&byte))
}

fn table_name(sandbox_id: Uuid) -> String {
    format!("ar_{}", sandbox_id.simple())
}

fn nft_rules(table_name: &str, spec: &NetworkSpec) -> String {
    let tap = &spec.tap_device;
    let uplink = &spec.uplink;
    let guest = spec.guest_ipv4;
    let mut rules = format!(
        "add table inet {table_name}\n\
         add chain inet {table_name} forward {{ type filter hook forward priority filter; policy accept; }}\n\
         add chain inet {table_name} input {{ type filter hook input priority filter; policy accept; }}\n\
         add table ip {table_name}\n\
         add chain ip {table_name} postrouting {{ type nat hook postrouting priority srcnat; policy accept; }}\n\
         add rule inet {table_name} forward iifname \"{uplink}\" oifname \"{tap}\" ip daddr {guest} ct state established,related accept\n\
         add rule inet {table_name} input iifname \"{tap}\" ct state established,related accept\n"
    );
    if spec.allow_egress {
        rules.push_str(&format!(
            "add rule inet {table_name} forward iifname \"{tap}\" oifname \"{uplink}\" ip saddr {guest} ct state new,established,related accept\n\
             add rule ip {table_name} postrouting oifname \"{uplink}\" ip saddr {guest} masquerade\n"
        ));
    }
    rules.push_str(&format!(
        "add rule inet {table_name} forward iifname \"{tap}\" drop\n\
         add rule inet {table_name} forward oifname \"{tap}\" drop\n\
         add rule inet {table_name} input iifname \"{tap}\" drop\n"
    ));
    rules
}

#[cfg(test)]
mod tests {
    use super::*;

    #[derive(Default)]
    struct FakeRunner {
        calls: Vec<(String, Vec<String>, Option<Vec<u8>>)>,
    }

    impl CommandRunner for FakeRunner {
        fn run(
            &mut self,
            program: &str,
            args: &[String],
            stdin: Option<&[u8]>,
        ) -> Result<(), NetworkError> {
            self.calls
                .push((program.into(), args.to_vec(), stdin.map(ToOwned::to_owned)));
            Ok(())
        }
    }

    fn spec(allow_egress: bool) -> NetworkSpec {
        NetworkSpec {
            sandbox_id: Uuid::nil(),
            tap_device: "tap-ar0".into(),
            host_ipv4: Ipv4Addr::new(172, 30, 0, 1),
            guest_ipv4: Ipv4Addr::new(172, 30, 0, 2),
            prefix_len: 30,
            uplink: "en0".into(),
            allow_egress,
        }
    }

    #[test]
    fn provisions_default_deny_network_with_nat() {
        let mut manager = NetworkManager::new(FakeRunner::default());
        let handle = manager.provision(&spec(false)).unwrap();
        let calls = &manager.runner.calls;
        assert_eq!(calls.len(), 4);
        let nft = String::from_utf8(calls[3].2.clone().unwrap()).unwrap();
        assert!(nft.contains("iifname \"tap-ar0\" drop"));
        assert!(!nft.contains("policy drop"));
        assert!(!nft.contains("masquerade"));
        assert!(nft.contains("input iifname \"tap-ar0\" ct state established,related accept"));
        assert!(!nft.contains("ct state new,established,related accept"));
        assert_eq!(handle.table_name, "ar_00000000000000000000000000000000");
    }

    #[test]
    fn adds_egress_rule_only_when_enabled() {
        let mut manager = NetworkManager::new(FakeRunner::default());
        manager.provision(&spec(true)).unwrap();
        let nft = String::from_utf8(manager.runner.calls[3].2.clone().unwrap()).unwrap();
        assert!(nft.contains("ct state new,established,related accept"));
        assert!(nft.contains("masquerade"));
    }

    #[test]
    fn slot_addresses_are_unique() {
        assert_ne!(
            addresses_for_slot(1).unwrap(),
            addresses_for_slot(2).unwrap()
        );
        assert!(addresses_for_slot(16_384).is_err());
    }

    #[test]
    fn rejects_shell_metacharacters_in_interface_names() {
        let mut network_spec = spec(false);
        network_spec.tap_device = "tap;rm".into();
        let mut manager = NetworkManager::new(FakeRunner::default());
        assert!(matches!(
            manager.provision(&network_spec),
            Err(NetworkError::InvalidSpec(_))
        ));
    }

    struct MissingResources;
    impl CommandRunner for MissingResources {
        fn run(
            &mut self,
            program: &str,
            _: &[String],
            _: Option<&[u8]>,
        ) -> Result<(), NetworkError> {
            Err(NetworkError::CommandFailed {
                program: program.into(),
                stderr: if program == "ip" {
                    "Cannot find device ar-test".into()
                } else {
                    "Could not process rule: No such file or directory".into()
                },
            })
        }
    }

    #[test]
    fn cleanup_already_absent_resources_succeeds() {
        let mut manager = NetworkManager::new(MissingResources);
        manager
            .cleanup(&NetworkHandle {
                tap_device: "ar-test".into(),
                table_name: "ar_test".into(),
                guest_ipv4: Ipv4Addr::LOCALHOST,
            })
            .unwrap();
    }

    #[test]
    fn connections_are_deleted_only_for_the_guest_in_both_directions() {
        let mut manager = NetworkManager::new(FakeRunner::default());
        manager.clear_connections(spec(false).guest_ipv4).unwrap();
        assert_eq!(manager.runner.calls.len(), 2);
        for (index, selector) in ["--orig-src", "--orig-dst"].iter().enumerate() {
            let call = &manager.runner.calls[index];
            assert_eq!(call.0, "conntrack");
            assert_eq!(
                call.1,
                ["--delete", "--family", "ipv4", selector, "172.30.0.2"]
            );
        }
    }
}
