//! Common functions and types for writing end-to-end tests of the parser CLI.

use parser::{Input, ParseConfig};
use serde_json::Value;
use std::fs::File;

use tempdir::TempDir;

pub fn input_for_file(rel_path: &str) -> Input {
    let file = File::open(rel_path).expect("failed to open file");
    Box::new(file)
}

pub fn input_bytes(content: impl AsRef<[u8]>) -> Input {
    let bytes = content.as_ref().to_vec();
    Box::new(std::io::Cursor::new(bytes))
}

pub struct CommandResult {
    pub parsed: Vec<Value>,
    pub raw_stdout: String,
    pub exit_code: i32,
}

pub fn run_test(config: &ParseConfig, mut input: Input) -> CommandResult {
    use std::io::BufRead;
    use std::process::{Command, Stdio};

    let tmp = TempDir::new("jsonl-parser-test").unwrap();
    let cfg_path = tmp.path().join("config.json");
    let mut cfg_file = File::create(&cfg_path).unwrap();
    serde_json::to_writer_pretty(&mut cfg_file, config).expect("failed to write config");
    std::mem::drop(cfg_file);

    let mut process = Command::new("./target/debug/parser")
        .args(&["parse", "--config-file", cfg_path.to_str().unwrap()])
        .stdin(Stdio::piped())
        .stderr(Stdio::piped())
        .stdout(Stdio::piped())
        .env("PARSER_LOG", "parser=debug")
        .spawn()
        .expect("failed to spawn parser process");

    let copy_result = std::io::copy(&mut input, &mut process.stdin.take().unwrap());
    let output = process
        .wait_with_output()
        .expect("failed to await completion of process");
    // Unwrap copy_result only after the process has completed, since wait_with_output is likely to
    // give us a more relevant error message.
    copy_result.expect("failed to copy input to stdin");

    // Code will be None if child exited due to a signal, so this is just to make debugging easier.
    let exit_code = output.status.code().unwrap_or_else(|| {
        println!("child process exited abnormally: {:?}", output.status);
        -1
    });
    let mut parsed = Vec::new();
    for line in output.stdout.lines() {
        println!("parser output line: {:?}", line);
        parsed.push(
            serde_json::from_str(&line.unwrap()).expect("failed to deserialize parser output"),
        );
    }
    let raw_stdout = String::from_utf8_lossy(&output.stdout).into_owned();

    // Print stderr so that it will show up in the output if the test fails.
    let stderr = String::from_utf8_lossy(&output.stderr).into_owned();
    println!("parser stderr:\n{}", stderr);

    CommandResult {
        parsed,
        exit_code,
        raw_stdout,
    }
}