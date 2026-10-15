fn main() -> Result<(), Box<dyn std::error::Error>> {
    tonic_prost_build::configure()
        .build_server(true)
        .build_client(false)
        .compile_protos(
            &[
                "../../../api/proto/agentruntime/v1/runtime.proto",
                "../../../api/proto/agentruntime/v1/guest.proto",
            ],
            &["../../../api/proto"],
        )?;
    println!("cargo:rerun-if-changed=../../../api/proto/agentruntime/v1/runtime.proto");
    println!("cargo:rerun-if-changed=../../../api/proto/agentruntime/v1/guest.proto");
    Ok(())
}
