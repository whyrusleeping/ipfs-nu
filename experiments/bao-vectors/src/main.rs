// Independent 16 KiB-group oracle. Data byte i = i % 251.
use bao_tree::{io::{outboard::PreOrderOutboard, round_up_to_chunks,
    sync::{CreateOutboard, encode_ranges_validated}}, BlockSize, ByteRanges};
use serde_json::json;
use std::io;

fn main() -> io::Result<()> {
    let mut vectors = Vec::new();
    for size in [0usize, 1, 1023, 1024, 16383, 16384, 16385, 32768, 32769, 278535] {
        let data: Vec<u8> = (0..size).map(|i| (i % 251) as u8).collect();
        let ob = PreOrderOutboard::<Vec<u8>>::create(io::Cursor::new(&data), BlockSize::from_chunk_log(4))?;
        let mut outboard = (size as u64).to_le_bytes().to_vec();
        outboard.extend_from_slice(&ob.data);
        let mut slices = Vec::new();
        let groups = ((size + 16383) / 16384).max(1);
        for (start, end) in [(0, groups), (0, 1), (groups - 1, groups)] {
            let ranges = round_up_to_chunks(&ByteRanges::from(
                (start as u64 * 16384)..(end as u64 * 16384)));
            let mut encoded = (size as u64).to_le_bytes().to_vec();
            encode_ranges_validated(&data[..], &ob, &ranges, &mut encoded)?;
            slices.push(json!({"start":start,"end":end,"hash":blake3::hash(&encoded).to_hex().to_string(),"bytes":encoded.len()}));
        }
        vectors.push(json!({"size":size,"root":ob.root.to_hex().to_string(),
            "outboard_hash":blake3::hash(&outboard).to_hex().to_string(),"slices":slices}));
    }
    println!("{}", serde_json::to_string_pretty(&vectors)?);
    Ok(())
}
